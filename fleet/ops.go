package fleet

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/gitx"
	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/tmux"
	"github.com/aldrinleal/agent-manager-public/util"
)

// List returns sessions, reconciling first so what comes back is current
// rather than whatever was last written.
func (f *Fleet) List(ctx context.Context, filter registry.Filter, reconcile bool) ([]*registry.Session, error) {
	if reconcile {
		if err := f.Reconcile(ctx); err != nil {
			// A host being unreachable should degrade the listing, not deny it.
			log.Warnf("fleet: list reconcile: %v", err)
		}
	}

	return f.reg.List(ctx, filter)
}

// Get resolves a session by id or task id.
func (f *Fleet) Get(ctx context.Context, ref string) (*registry.Session, error) {
	return f.reg.Lookup(ctx, ref)
}

// Log returns the tail of a session's output.
//
// capture-pane first, the log file only as a fallback. That ordering matters
// more than it looks: an agent like Claude Code is a full-screen TUI, so the
// pipe-pane log is a stream of terminal *redraws*, not a transcript —
// tailing it yields the same few sentences re-emitted a hundred times,
// interleaved with spinner frames. capture-pane returns the rendered screen
// plus scrollback, which is what a human actually wants to read.
//
// The log file still earns its keep: it survives the session (capture-pane
// returns nothing once tmux is gone) and it is a second source for the URL
// scrape when the banner has already scrolled off the pane.
func (f *Fleet) Log(ctx context.Context, ref string, lines int) (string, error) {
	session, err := f.reg.Lookup(ctx, ref)
	if err != nil {
		return "", err
	}

	if lines <= 0 {
		lines = 200
	}

	t, err := f.transportFor(session.Host)
	if err != nil {
		return "", err
	}

	execCtx, cancel := f.ctxWithExecTimeout(ctx)
	defer cancel()

	if session.State.Active() {
		res, cErr := t.Run(execCtx, tmux.CapturePaneCmd(session.TmuxSession, lines))
		if cErr != nil {
			return "", cErr
		}

		if out := strings.TrimSpace(util.StripANSI(res.Stdout)); out != "" {
			return out + "\n", nil
		}
	}

	res, err := t.Run(execCtx, fmt.Sprintf("tail -n %d %s 2>/dev/null", lines, util.ShellQuote(session.LogPath)))
	if err != nil {
		return "", err
	}

	return util.StripANSI(res.Stdout), nil
}

// Send types text into a running session and presses Enter — a new
// instruction without going through Remote Control.
func (f *Fleet) Send(ctx context.Context, ref, text string) error {
	session, err := f.reg.Lookup(ctx, ref)
	if err != nil {
		return err
	}

	if !session.State.Active() {
		return ErrConflict.New("session %s is %s — nothing to send to", session.ID, session.State)
	}

	t, err := f.transportFor(session.Host)
	if err != nil {
		return err
	}

	execCtx, cancel := f.ctxWithExecTimeout(ctx)
	defer cancel()

	return tmux.SendKeys(execCtx, t, session.TmuxSession, text)
}

// AttachInfo is what a human needs to take a session over by hand.
//
// An ssh host is reached in two steps, SSHCommand then TmuxCommand on the far
// side. A kubectl host is one: ExecCommand runs tmux attach inside the pod,
// and SSHCommand is empty. Steps gives whichever applies.
type AttachInfo struct {
	Session     string           `json:"session"`
	Host        string           `json:"host"`
	Transport   config.Transport `json:"transport"`
	Address     string           `json:"address,omitempty"`
	Jump        string           `json:"jump,omitempty"`
	SSHCommand  string           `json:"ssh_command"`
	TmuxCommand string           `json:"tmux_command"`
	ExecCommand string           `json:"exec_command,omitempty"`
	Worktree    string           `json:"worktree"`
	LogPath     string           `json:"log_path"`
	RemoteURL   string           `json:"remote_url,omitempty"`
}

// Attach returns the commands to reach a session directly.
//
// Deliberately more than the Remote Control URL: if that connection drops,
// these still work (need.md, "Alternativa: URLs não são necessárias").
func (f *Fleet) Attach(ctx context.Context, ref string) (*AttachInfo, error) {
	session, err := f.reg.Lookup(ctx, ref)
	if err != nil {
		return nil, err
	}

	host, err := f.inv.Host(session.Host)
	if err != nil {
		return nil, err
	}

	info := &AttachInfo{
		Session:     session.TmuxSession,
		Host:        host.Name,
		Transport:   host.Transport,
		Address:     host.Address,
		Jump:        host.Jump,
		TmuxCommand: tmux.AttachCommand(session.TmuxSession),
		Worktree:    session.Worktree,
		LogPath:     session.LogPath,
		RemoteURL:   session.RemoteURL,
	}

	if host.IsKubectl() {
		info.ExecCommand = kubectlCommand(host) + " " + info.TmuxCommand
	} else {
		info.SSHCommand = sshCommand(f.inv, host)
	}

	return info, nil
}

// Steps are the commands a human runs, in order, to reach the session.
func (a *AttachInfo) Steps() []string {
	if a.ExecCommand != "" {
		return []string{a.ExecCommand}
	}

	return []string{a.SSHCommand, a.TmuxCommand}
}

// kubectlCommand renders the `kubectl exec -it` prefix for a workspace pod.
// Like sshCommand it is only for a human: agent-manager speaks pods/exec
// through client-go and never runs kubectl itself.
func kubectlCommand(host *config.Host) string {
	cmd := "kubectl -n " + host.Namespace + " exec -it " + host.Pod
	if host.Container != "" {
		cmd += " -c " + host.Container
	}

	return cmd + " --"
}

// sshCommand renders the equivalent openssh invocation, walking the jump
// chain into -J. agent-manager itself never shells out to ssh — this exists
// purely so a human can reproduce the hop by hand.
func sshCommand(inv *config.Inventory, host *config.Host) string {
	var jumps []string

	for cur := host; cur.Jump != ""; {
		next, err := inv.Host(cur.Jump)
		if err != nil {
			break
		}

		jumps = append(jumps, hostSpec(next))
		cur = next
	}

	var b strings.Builder

	b.WriteString("ssh ")

	if len(jumps) > 0 {
		b.WriteString("-J " + strings.Join(jumps, ",") + " ")
	}

	if _, port, ok := splitPort(host.Addr); ok && port != "22" {
		b.WriteString("-p " + port + " ")
	}

	b.WriteString(host.User + "@" + hostOnly(host.Addr))

	return b.String()
}

func hostSpec(h *config.Host) string {
	if _, port, ok := splitPort(h.Addr); ok && port != "22" {
		return h.User + "@" + hostOnly(h.Addr) + ":" + port
	}

	return h.User + "@" + hostOnly(h.Addr)
}

func splitPort(addr string) (host, port string, ok bool) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr, "", false
	}

	return addr[:i], addr[i+1:], true
}

func hostOnly(addr string) string {
	h, _, ok := splitPort(addr)
	if !ok {
		return addr
	}

	return strings.Trim(h, "[]")
}

// Kill stops a session, optionally removing its worktree.
//
// The worktree is kept by default: it holds the agent's work, and throwing
// that away on a stop would be the wrong default in every case where the
// agent got something right.
func (f *Fleet) Kill(ctx context.Context, ref string, removeWorktree bool) (*registry.Session, error) {
	session, err := f.reg.Lookup(ctx, ref)
	if err != nil {
		return nil, err
	}

	t, err := f.transportFor(session.Host)
	if err != nil {
		return nil, err
	}

	execCtx, cancel := f.ctxWithExecTimeout(ctx)
	defer cancel()

	if err := tmux.KillSession(execCtx, t, session.TmuxSession); err != nil {
		return nil, err
	}

	if removeWorktree && session.Worktree != session.RepoDir {
		if err := gitx.RemoveWorktree(execCtx, t, session.RepoDir, session.Worktree, true); err != nil {
			// The session is already dead; report the worktree problem without
			// pretending the kill failed.
			session.Note = "killed, but removing worktree failed: " + err.Error()
			log.Warnf("fleet: %v", err)
		}
	}

	now := time.Now().UTC()

	session.State = registry.StateStopped
	session.EndedAt = &now

	if session.Note == "" {
		session.Note = "killed by operator"
	}

	if err := f.reg.Update(ctx, session); err != nil {
		return nil, err
	}

	log.Infof("fleet: killed %s on %s", session.TmuxSession, session.Host)

	return session, nil
}

// Forget removes a session row without touching the host.
func (f *Fleet) Forget(ctx context.Context, ref string) error {
	session, err := f.reg.Lookup(ctx, ref)
	if err != nil {
		return err
	}

	return f.reg.Delete(ctx, session.ID)
}

// Refresh brings a checkout up to date.
func (f *Fleet) Refresh(
	ctx context.Context,
	hostName, pathName string,
	o gitx.RefreshOpts,
) (*gitx.RefreshResult, error) {
	target, err := f.ResolveTarget(hostName, pathName)
	if err != nil {
		return nil, err
	}

	if err := f.ensureChecked(ctx, target.Host.Name); err != nil {
		return nil, err
	}

	t, err := f.transportFor(target.Host.Name)
	if err != nil {
		return nil, err
	}

	// Fetching a large repo takes longer than an ordinary command.
	execCtx, cancel := context.WithTimeout(ctx, 10*f.cfg.ExecTimeout)
	defer cancel()

	result, err := gitx.RefreshWith(execCtx, t, target.Path, o)
	if result != nil {
		result.Host = target.Host.Name
	}

	return result, err
}

// RefreshHost refreshes every path on a host, reporting each result.
func (f *Fleet) RefreshHost(
	ctx context.Context,
	hostName string,
	o gitx.RefreshOpts,
) ([]*gitx.RefreshResult, error) {
	host, err := f.inv.Host(hostName)
	if err != nil {
		return nil, err
	}

	var (
		out      []*gitx.RefreshResult
		firstErr error
	)

	for i := range host.Paths {
		result, rErr := f.Refresh(ctx, hostName, host.Paths[i].Name, o)

		if result == nil {
			result = &gitx.RefreshResult{Host: hostName, Path: host.Paths[i].Name}
		}

		if rErr != nil {
			result.Message = rErr.Error()

			if firstErr == nil {
				firstErr = rErr
			}
		}

		out = append(out, result)
	}

	return out, firstErr
}
