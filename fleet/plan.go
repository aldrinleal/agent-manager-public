package fleet

import (
	"context"
	"fmt"
	"strings"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/gitx"
	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/transport"
)

// launchCtx is everything Launch decides before its first write.
type launchCtx struct {
	target    *Target
	names     *names
	workspace config.Workspace
	existing  []*registry.Session
}

// resolveLaunch validates a request and works out what it refers to: the
// allowlisted target, every derived name, the workspace mode, and any
// sessions already recorded for the same (host, repo, task).
//
// It performs no remote command and no write. Both Plan and Launch start here.
func (f *Fleet) resolveLaunch(ctx context.Context, req LaunchRequest) (*launchCtx, error) {
	target, err := f.ResolveTarget(req.Host, req.Path)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(req.Prompt) == "" {
		return nil, ErrLaunch.New("a prompt is required")
	}

	n, err := f.namesFor(target, req.TaskID)
	if err != nil {
		return nil, err
	}

	workspace := target.Path.Workspace

	if req.Workspace != "" {
		workspace = req.Workspace

		// Recompute the paths that depend on the workspace mode.
		scratch := *target.Path
		scratch.Workspace = workspace

		if n, err = f.namesFor(&Target{Host: target.Host, Path: &scratch}, req.TaskID); err != nil {
			return nil, err
		}
	}

	// An existing row for this (host, repo, task) means a relaunch. Hand the
	// caller what is already running rather than starting a second agent on
	// the same branch.
	existing, err := f.reg.List(ctx, registry.Filter{
		Host: target.Host.Name, RepoName: target.Path.Name, TaskID: n.TaskID,
	})
	if err != nil {
		return nil, err
	}

	return &launchCtx{target: target, names: n, workspace: workspace, existing: existing}, nil
}

// LaunchPlan is what a launch WOULD do, for a human to confirm first.
type LaunchPlan struct {
	Host      string           `json:"host"`
	Path      string           `json:"path"`
	RepoDir   string           `json:"repo_dir"`
	TaskID    string           `json:"task_id"`
	Runner    string           `json:"runner"`
	Workspace config.Workspace `json:"workspace"`

	// Base is the branch the worktree is cut from; Branch is the one created.
	// In an in-place launch they are the same: nothing new is created.
	Base   string `json:"base"`
	Branch string `json:"branch"`

	Worktree   string `json:"worktree"`
	LogPath    string `json:"log_path"`
	PromptPath string `json:"prompt_path"`
	Session    string `json:"tmux_session"`

	PromptPreview string `json:"prompt_preview"`

	// Conflict is the active session that makes Launch answer 409 unless
	// force is set. WillReplace says force was set and it would be killed.
	Conflict    *registry.Session `json:"conflict,omitempty"`
	WillReplace bool              `json:"will_replace"`

	// WorktreeExists: the worktree directory is already there and will be
	// REUSED, with whatever is in it — not recreated.
	WorktreeExists bool `json:"worktree_exists"`
	// Dirty lists tracked changes in the checkout the agent will run in
	// (the existing worktree, or the repo itself for an in-place launch).
	Dirty []string `json:"dirty,omitempty"`

	Preflight *registry.HostCheck `json:"preflight,omitempty"`
	Warnings  []string            `json:"warnings"`

	// Blocked is true when Launch would refuse outright (an unforced
	// conflict). A confirmation prompt must not offer to proceed.
	Blocked bool `json:"blocked"`
}

// Plan reports what Launch would do without doing any of it.
//
// Remote access is read-only: `test -e`, `git rev-parse`, `git status` and
// the cached preflight row. Nothing is written to the host, no session row is
// created, and the preflight cache is read but never refreshed (a refresh is
// a write to our own registry, and a stale cache is reported as a warning
// instead). Used for "always confirm before launching".
func (f *Fleet) Plan(ctx context.Context, req LaunchRequest) (*LaunchPlan, error) {
	lc, err := f.resolveLaunch(ctx, req)
	if err != nil {
		return nil, err
	}

	t, n, ws := lc.target, lc.names, lc.workspace

	base := t.Path.Branch
	if req.Branch != "" {
		base = req.Branch
	}

	plan := &LaunchPlan{
		Host:          t.Host.Name,
		Path:          t.Path.Name,
		RepoDir:       t.Path.Dir,
		TaskID:        n.TaskID,
		Runner:        t.Path.Runner,
		Workspace:     ws,
		Base:          base,
		Branch:        n.Branch,
		Worktree:      n.Worktree,
		LogPath:       n.LogPath,
		PromptPath:    n.PromptPath,
		Session:       n.Session,
		PromptPreview: preview(req.Prompt),
		Warnings:      []string{},
	}

	if ws == config.WorkspaceInplace {
		plan.Branch = base
		plan.Warnings = append(plan.Warnings,
			"in-place: the agent works directly in the checkout, not an isolated worktree")
	}

	for _, s := range lc.existing {
		if s.State.Active() {
			plan.Conflict = s
			plan.WillReplace = req.Force
			plan.Blocked = !req.Force
		}
	}

	check, err := f.reg.HostCheck(ctx, t.Host.Name)
	if err != nil {
		return nil, err
	}

	plan.Preflight = check

	switch {
	case check == nil:
		plan.Warnings = append(plan.Warnings, "host has not been preflighted yet; launch will run preflight first")
	case !check.OK:
		plan.Warnings = append(plan.Warnings, "last preflight FAILED: "+check.Note)
	}

	tr, err := f.transportFor(t.Host.Name)
	if err != nil {
		plan.Warnings = append(plan.Warnings, "host unreachable, so the remote checks below were skipped: "+err.Error())
		return plan, nil
	}

	execCtx, cancel := f.ctxWithExecTimeout(ctx)
	defer cancel()

	f.planRemote(execCtx, plan, tr, t, ws, base)

	return plan, nil
}

// planRemote adds what only the host can say. Every command here is a read.
func (f *Fleet) planRemote(ctx context.Context, plan *LaunchPlan, tr transport.Transport, t *Target, ws config.Workspace, base string) {
	if ws == config.WorkspaceWorktree {
		exists, err := gitx.PathExists(ctx, tr, plan.Worktree)
		if err != nil {
			plan.Warnings = append(plan.Warnings, "could not check the worktree: "+err.Error())
		}

		plan.WorktreeExists = exists

		if exists {
			plan.Warnings = append(plan.Warnings,
				"the worktree already exists and will be REUSED with whatever is in it")
		}
	}

	// The base must exist as <remote>/<base> after the fetch Launch does. A
	// stale remote-tracking ref is the best a read-only plan can check; it
	// still catches a typo, which is the usual failure.
	if ok, err := gitx.RefExists(ctx, tr, t.Path.Dir, t.Path.Remote+"/"+base); err == nil && !ok {
		plan.Warnings = append(plan.Warnings,
			fmt.Sprintf("%s/%s was not found locally; the launch fetches first, but check the base branch name", t.Path.Remote, base))
	}

	// The checkout the agent will actually run in.
	dir := t.Path.Dir
	if ws == config.WorkspaceWorktree && plan.WorktreeExists {
		dir = plan.Worktree
	}

	if ws == config.WorkspaceInplace || plan.WorktreeExists {
		if st, err := gitx.Status(ctx, tr, dir, false); err == nil && st != "" {
			plan.Dirty = strings.Split(st, "\n")
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("%d tracked change(s) already in %s — the agent starts on top of them", len(plan.Dirty), dir))
		}
	}
}

// RefreshPlanItem is what a refresh of one checkout would do.
type RefreshPlanItem struct {
	Host     string          `json:"host"`
	Path     string          `json:"path"`
	Dir      string          `json:"dir"`
	Remote   string          `json:"remote"`
	Branch   string          `json:"branch"`
	Strategy config.Strategy `json:"strategy"`
	Head     string          `json:"head,omitempty"`
	Ahead    int             `json:"ahead"`
	Behind   int             `json:"behind"`

	// Dirty lists tracked, modified files (untracked ones do not count, as in
	// gitx.Refresh). WillStash is true when autostash will set them aside
	// around a rebase/merge. WouldBlock is true when the refresh would be
	// refused — the confirmation prompt must not offer to proceed.
	Dirty      []string `json:"dirty,omitempty"`
	WillStash  bool     `json:"will_stash"`
	InProgress string   `json:"in_progress,omitempty"`
	WouldBlock bool     `json:"would_block"`
	Warnings   []string `json:"warnings"`
}

// PlanRefresh reports what Refresh would do to one path, or to every path on
// the host when pathName is empty. Read-only, like Plan.
func (f *Fleet) PlanRefresh(ctx context.Context, hostName, pathName string, o gitx.RefreshOpts) ([]RefreshPlanItem, error) {
	host, err := f.inv.Host(hostName)
	if err != nil {
		return nil, err
	}

	var targets []*Target

	if pathName == "" {
		for i := range host.Paths {
			targets = append(targets, &Target{Host: host, Path: &host.Paths[i]})
		}
	} else {
		t, rErr := f.ResolveTarget(hostName, pathName)
		if rErr != nil {
			return nil, rErr
		}

		targets = append(targets, t)
	}

	tr, err := f.transportFor(host.Name)
	if err != nil {
		return nil, err
	}

	execCtx, cancel := f.ctxWithExecTimeout(ctx)
	defer cancel()

	items := make([]RefreshPlanItem, 0, len(targets))

	for _, t := range targets {
		s := o.Strategy
		if s == "" {
			s = t.Path.Refresh
		}

		it := RefreshPlanItem{
			Host: host.Name, Path: t.Path.Name, Dir: t.Path.Dir,
			Remote: t.Path.Remote, Branch: t.Path.Branch, Strategy: s,
			Warnings: []string{},
		}

		st, sErr := gitx.GetRepoStatus(execCtx, tr, t.Path.Dir)
		if sErr != nil {
			it.WouldBlock = true
			it.Warnings = append(it.Warnings, sErr.Error())
			items = append(items, it)

			continue
		}

		it.Head, it.Ahead, it.Behind, it.InProgress = st.Head, st.Ahead, st.Behind, st.InProgress

		// Mirrors gitx.RefreshWith: a stopped operation blocks first, then dirt.
		if st.InProgress != "" {
			it.WouldBlock = true
			it.Warnings = append(it.Warnings,
				fmt.Sprintf("stopped mid-%s: the refresh would be REFUSED until a human resolves or aborts it", st.InProgress))
		}

		if len(st.Dirty) > 0 {
			it.Dirty = st.Dirty
			rebaseOrMerge := s == config.StrategyRebase || s == config.StrategyMerge

			switch {
			case o.Autostash && rebaseOrMerge:
				it.WillStash = true
				it.Warnings = append(it.Warnings,
					fmt.Sprintf("%d dirty file(s) will be stashed and re-applied; if re-applying conflicts, git keeps the stash (never dropped)", len(st.Dirty)))
			case o.Autostash:
				it.Warnings = append(it.Warnings,
					fmt.Sprintf("%d dirty file(s) left in place; %s cannot lose them, and git refuses by itself if an incoming change would overwrite one", len(st.Dirty), s))
			case o.Force || t.Path.AllowDirty:
				it.Warnings = append(it.Warnings,
					fmt.Sprintf("%d dirty file(s) and no autostash: git may refuse, or stop mid-operation", len(st.Dirty)))
			default:
				it.WouldBlock = true
				it.Warnings = append(it.Warnings,
					fmt.Sprintf("%d dirty file(s): the refresh would be REFUSED (use autostash, force, or allow_dirty)", len(st.Dirty)))
			}
		}

		if s == config.StrategyRebase || s == config.StrategyMerge {
			it.Warnings = append(it.Warnings,
				"a conflict is left mid-operation on purpose for a human to resolve; it is never aborted")
		}

		items = append(items, it)
	}

	return items, nil
}
