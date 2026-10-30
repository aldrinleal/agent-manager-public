package fleet

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/gitx"
	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/runner"
	"github.com/aldrinleal/agent-manager-public/tmux"
	"github.com/aldrinleal/agent-manager-public/transport"
	"github.com/aldrinleal/agent-manager-public/util"
)

// LaunchRequest is what the CLI and the API both submit.
type LaunchRequest struct {
	Host      string           `json:"host"`
	Path      string           `json:"path"`
	TaskID    string           `json:"task_id"`
	Prompt    string           `json:"prompt"`
	Branch    string           `json:"branch,omitempty"`    // base branch; defaults to the path's
	Workspace config.Workspace `json:"workspace,omitempty"` // overrides the path's
	ExtraArgs []string         `json:"extra_args,omitempty"`
	Force     bool             `json:"force,omitempty"`
}

// Launch starts an agent session and records it.
//
// It returns as soon as tmux is running — the Remote Control URL arrives
// asynchronously and is written to the registry when it does. A session
// without a URL is still a perfectly usable session (tmux attach, the log),
// which is why the URL is never allowed to fail the launch (need.md: "o
// status real não deve depender só da URL").
func (f *Fleet) Launch(ctx context.Context, req LaunchRequest) (*registry.Session, error) {
	// Everything up to the first write is shared with Plan (plan.go), so the
	// preview a human confirms cannot disagree with what then runs.
	lc, err := f.resolveLaunch(ctx, req)
	if err != nil {
		return nil, err
	}

	target, n, workspace, existing := lc.target, lc.names, lc.workspace, lc.existing

	for _, s := range existing {
		if s.State.Active() && !req.Force {
			return s, ErrConflict.New(
				"task %q is already %s on %s/%s (session %s) — pass --force to replace it",
				n.TaskID, s.State, s.Host, s.RepoName, s.ID)
		}
	}

	select {
	case f.launchSem <- struct{}{}:
		defer func() { <-f.launchSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if err := f.ensureChecked(ctx, target.Host.Name); err != nil {
		return nil, err
	}

	t, err := f.transportFor(target.Host.Name)
	if err != nil {
		return nil, err
	}

	execCtx, cancel := context.WithTimeout(ctx, f.cfg.ExecTimeout)
	defer cancel()

	// A live tmux session with this name that we did not just find in the
	// registry means something outside agent-manager owns it. Never clobber it.
	occupied, err := tmux.HasSession(execCtx, t, n.Session)
	if err != nil {
		return nil, err
	}

	if occupied {
		if !req.Force {
			return nil, ErrConflict.New(
				"tmux session %q already exists on %s — pass --force to kill and relaunch it",
				n.Session, target.Host.Name)
		}

		if err := tmux.KillSession(execCtx, t, n.Session); err != nil {
			return nil, err
		}
	}

	session := &registry.Session{
		ID:            uuid.NewString(),
		TaskID:        n.TaskID,
		Runner:        target.Path.Runner,
		Host:          target.Host.Name,
		RepoName:      target.Path.Name,
		RepoDir:       target.Path.Dir,
		Worktree:      n.Worktree,
		Branch:        target.Path.Branch,
		TmuxSession:   n.Session,
		LogPath:       n.LogPath,
		State:         registry.StateStarting,
		PromptPreview: preview(req.Prompt),
	}

	if workspace == config.WorkspaceWorktree {
		session.Branch = n.Branch
	}

	// Replace any stopped row for the same key, so the unique index holds and
	// `agentmgr ls` does not accumulate one dead row per relaunch.
	for _, s := range existing {
		if err := f.reg.Delete(ctx, s.ID); err != nil {
			return nil, err
		}
	}

	if err := f.prepare(execCtx, t, target, n, workspace, req); err != nil {
		session.State = registry.StateFailed
		session.Note = err.Error()
		_ = f.reg.Create(ctx, session)

		return session, err
	}

	if err := f.start(execCtx, t, target, n); err != nil {
		session.State = registry.StateFailed
		session.Note = err.Error()
		_ = f.reg.Create(ctx, session)

		return session, err
	}

	if err := f.reg.Create(ctx, session); err != nil {
		return nil, err
	}

	log.Infof("fleet: launched %s on %s (%s, branch %s)",
		n.Session, target.Host.Name, n.Worktree, session.Branch)

	// The URL scrape outlives this request. It gets its own context so an
	// `agentmgr up` that the user Ctrl-C's does not abandon the session.
	go f.scrapeURL(session)

	return session, nil
}

// prepare makes the workspace and writes the prompt and the launch scripts.
func (f *Fleet) prepare(
	ctx context.Context,
	t transport.Transport,
	target *Target,
	n *names,
	workspace config.Workspace,
	req LaunchRequest,
) error {
	base := target.Path.Branch
	if req.Branch != "" {
		base = req.Branch
	}

	if workspace == config.WorkspaceWorktree {
		if err := gitx.AddWorktree(ctx, t, target.Path.Dir, n.Worktree, n.Branch, target.Path.Remote, base); err != nil {
			return err
		}
	} else if err := gitx.IsRepo(ctx, t, target.Path.Dir); err != nil {
		return err
	}

	// The prompt goes down the session's stdin as raw bytes: it is never
	// quoted, never shell-interpreted, and never passes through tmux's parser
	// (SPEC.md §7.1). This is what makes a prompt containing quotes, newlines
	// or $(...) safe.
	if err := t.WriteFile(ctx, n.PromptPath, 0o600, strings.NewReader(req.Prompt)); err != nil {
		return err
	}

	r, err := runner.Get(target.Path.Runner)
	if err != nil {
		return err
	}

	script, err := r.LaunchScript(runner.LaunchSpec{
		SessionLabel: n.Label,
		Worktree:     n.Worktree,
		PromptFile:   n.PromptPath,
		Env:          target.Host.Env,
		ExtraArgs:    req.ExtraArgs,
	})
	if err != nil {
		return err
	}

	if target.Host.Capture == config.CaptureScript {
		// script(1) wraps the agent script, giving a pty-accurate log. Costs a
		// util-linux dependency, which is why it is not the default.
		if err := t.WriteFile(ctx, n.AgentPath, 0o700, strings.NewReader(script)); err != nil {
			return err
		}

		wrapper := "#!/bin/sh\n# Rendered by agent-manager (capture: script).\n" +
			"exec script -q -f " + util.ShellQuote(n.LogPath) + " -c " + util.ShellQuote(n.AgentPath) + "\n"

		return t.WriteFile(ctx, n.ScriptPath, 0o700, strings.NewReader(wrapper))
	}

	return t.WriteFile(ctx, n.ScriptPath, 0o700, strings.NewReader(script))
}

// start creates the tmux session and wires up logging.
//
// If anything after new-session fails, the session is killed rather than left
// running: a live agent recorded as "failed" is worse than no agent, because
// nothing is watching it and the next launch of the same task id just
// conflicts.
func (f *Fleet) start(ctx context.Context, t transport.Transport, target *Target, n *names) error {
	// NewSession also sets remain-on-exit, chained into the same tmux command
	// so a fast-failing agent cannot exit before the option applies.
	if err := tmux.NewSession(ctx, t, n.Session, n.Worktree, n.ScriptPath); err != nil {
		return err
	}

	if target.Host.Capture == config.CapturePipePane {
		if err := tmux.PipePane(ctx, t, n.Session, n.LogPath); err != nil {
			f.abandon(t, n.Session)

			return err
		}
	}

	return nil
}

// abandon kills a session left behind by a failed launch. It uses its own
// context: the caller's has usually just expired, and leaving the session
// running is the outcome being avoided.
func (f *Fleet) abandon(t transport.Transport, session string) {
	ctx, cancel := context.WithTimeout(context.Background(), f.cfg.ExecTimeout)
	defer cancel()

	if err := tmux.KillSession(ctx, t, session); err != nil {
		log.Warnf("fleet: launch failed and tmux session %s could not be cleaned up: %v", session, err)
	}
}

// scrapeURL polls the pane and the log for the runner's control URL.
//
// Failure to find one is not a launch failure — the session stays usable and
// the row simply has no remote_url.
func (f *Fleet) scrapeURL(session *registry.Session) {
	r, err := runner.Get(session.Runner)
	if err != nil || r.URLPattern() == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), f.cfg.URLScrapeTimeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		url := f.findURL(ctx, session, r.URLPattern().FindString)
		if url != "" {
			current, gErr := f.reg.Get(context.Background(), session.ID)
			if gErr != nil {
				return
			}

			current.RemoteURL = url
			if current.State == registry.StateStarting {
				current.State = registry.StateRunning
			}

			if uErr := f.reg.Update(context.Background(), current); uErr != nil {
				log.Warnf("fleet: recording URL for %s: %v", session.ID, uErr)
			}

			log.Infof("fleet: %s -> %s", session.TmuxSession, url)

			return
		}

		select {
		case <-ctx.Done():
			log.Warnf("fleet: no control URL for %s after %s — session is still usable via tmux/log",
				session.TmuxSession, f.cfg.URLScrapeTimeout)

			return
		case <-f.stop:
			return
		case <-ticker.C:
		}
	}
}

// findURL reads the pane and the log in one round trip and matches against
// both. capture-pane's -J is what makes a URL wrapped at the terminal width
// matchable at all (SPEC.md §16.1).
func (f *Fleet) findURL(ctx context.Context, session *registry.Session, match func(string) string) string {
	t, err := f.transportFor(session.Host)
	if err != nil {
		return ""
	}

	cmd := fmt.Sprintf("{ %s; tail -c 65536 %s 2>/dev/null; } 2>/dev/null",
		tmux.CapturePaneCmd(session.TmuxSession, 400),
		util.ShellQuote(session.LogPath))

	execCtx, cancel := context.WithTimeout(ctx, f.cfg.ExecTimeout)
	defer cancel()

	res, err := t.Run(execCtx, cmd)
	if err != nil {
		return ""
	}

	return match(util.StripANSI(res.Stdout))
}
