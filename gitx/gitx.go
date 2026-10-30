// Package gitx runs the git operations agent-manager needs on a remote host:
// bringing a checkout up to date, and carving a worktree out of it for an
// agent session.
//
// Nothing here forwards credentials. The manager's SSH key authenticates to
// the *host*; the host's own deploy key or credential helper authenticates to
// GitHub (SPEC.md §10).
package gitx

import (
	"context"
	"fmt"
	"strings"

	"github.com/joomcode/errorx"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/transport"
	"github.com/aldrinleal/agent-manager-public/util"
)

var Errors = errorx.NewNamespace("gitx")

var (
	ErrInProgress = Errors.NewType("in_progress")
	ErrGit        = Errors.NewType("git")
	ErrNotRepo    = Errors.NewType("not_a_repo")
	ErrDirty      = Errors.NewType("dirty")
	ErrConflict   = Errors.NewType("conflict")
)

// git builds a `git -C dir ...` command line with everything quoted.
func git(dir string, args ...string) string {
	return "git -C " + util.ShellQuote(dir) + " " + util.JoinArgs(args...)
}

// Version returns the remote git version, or an error if git is missing.
func Version(ctx context.Context, t transport.Transport) (string, error) {
	res, err := t.Run(ctx, "git --version")
	if err != nil {
		return "", err
	}

	if !res.OK() {
		return "", ErrGit.New("git not available (exit %d): %s", res.ExitCode, res.Err())
	}

	return res.Out(), nil
}

// IsRepo checks dir really is a git work tree before anything touches it.
func IsRepo(ctx context.Context, t transport.Transport, dir string) error {
	res, err := t.Run(ctx, git(dir, "rev-parse", "--is-inside-work-tree"))
	if err != nil {
		return err
	}

	if !res.OK() || res.Out() != "true" {
		return ErrNotRepo.New("%s is not a git work tree: %s", dir, res.Err())
	}

	return nil
}

// Status returns porcelain status; empty means clean.
//
// untracked selects whether "??" entries count. The dirty guard passes false:
// an untracked file cannot be lost by a fetch or a fast-forward, git pull
// itself tolerates them, and nearly every working checkout has some stray
// build output — treating those as dirty would block almost every refresh.
// (If an untracked file really would be overwritten by the merge, git refuses
// on its own, with a better message than we could write.)
func Status(ctx context.Context, t transport.Transport, dir string, untracked bool) (string, error) {
	mode := "no"
	if untracked {
		mode = "normal"
	}

	res, err := t.Run(ctx, git(dir, "status", "--porcelain", "--untracked-files="+mode))
	if err != nil {
		return "", err
	}

	if !res.OK() {
		return "", ErrGit.New("git status in %s failed (exit %d): %s", dir, res.ExitCode, res.Err())
	}

	return strings.TrimSpace(res.Stdout), nil
}

// Head returns the current commit SHA.
func Head(ctx context.Context, t transport.Transport, dir string) (string, error) {
	res, err := t.Run(ctx, git(dir, "rev-parse", "HEAD"))
	if err != nil {
		return "", err
	}

	if !res.OK() {
		return "", ErrGit.New("git rev-parse HEAD in %s failed (exit %d): %s", dir, res.ExitCode, res.Err())
	}

	return res.Out(), nil
}

// Fetch updates remote-tracking refs and prunes deleted ones.
func Fetch(ctx context.Context, t transport.Transport, dir, remote string) error {
	res, err := t.Run(ctx, git(dir, "fetch", "--prune", remote))
	if err != nil {
		return err
	}

	if !res.OK() {
		return ErrGit.New("git fetch %s in %s failed (exit %d): %s", remote, dir, res.ExitCode, res.Err())
	}

	return nil
}

// RefreshResult reports what a refresh did, in enough detail to be useful
// without re-running anything.
type RefreshResult struct {
	Host      string          `json:"host"`
	Path      string          `json:"path"`
	Dir       string          `json:"dir"`
	Strategy  config.Strategy `json:"strategy"`
	Remote    string          `json:"remote"`
	Branch    string          `json:"branch"`
	Before    string          `json:"before"`
	After     string          `json:"after"`
	Changed   bool            `json:"changed"`
	DiffStat  string          `json:"diffstat,omitempty"`
	Conflict  bool            `json:"conflict"`
	Conflicts []string        `json:"conflicts,omitempty"`
	WasDirty  bool            `json:"was_dirty"`
	Message   string          `json:"message,omitempty"`

	// Autostash is true when git was asked to stash and re-apply the dirty
	// tracked files around a rebase/merge.
	Autostash bool `json:"autostash,omitempty"`
	// Stashed is how many dirty tracked files were set aside.
	Stashed int `json:"stashed,omitempty"`
	// StashRef is the commit of a stash entry that is STILL THERE afterwards:
	// the operation stopped, or re-applying the stash conflicted, so git kept
	// it. The user's changes are safe in it; `git stash pop` once resolved. It
	// is never dropped on anyone's behalf.
	StashRef string `json:"stash_ref,omitempty"`
}

// RefreshOpts are the knobs of RefreshWith.
type RefreshOpts struct {
	Strategy config.Strategy
	// Force proceeds over uncommitted tracked changes without stashing them.
	Force bool
	// Autostash lets git stash dirty tracked files around a rebase or merge and
	// re-apply them afterwards (git's own --autostash). It has no effect on
	// ff-only or fetch-only, which are safe on a dirty tree anyway: a
	// fast-forward refuses by itself if it would overwrite a modified file.
	Autostash bool
}

// Refresh is RefreshWith without autostash.
func Refresh(
	ctx context.Context,
	t transport.Transport,
	p *config.Path,
	strategy config.Strategy,
	force bool,
) (*RefreshResult, error) {
	return RefreshWith(ctx, t, p, RefreshOpts{Strategy: strategy, Force: force})
}

// RefreshWith brings dir up to date with remote/branch.
//
// It is deliberately `fetch` + an explicit merge/rebase against
// <remote>/<branch> rather than `git pull`, so it does not depend on upstream
// tracking being configured and works on an oddly-tracked or detached
// checkout (SPEC.md §10).
func RefreshWith(
	ctx context.Context,
	t transport.Transport,
	p *config.Path,
	o RefreshOpts,
) (*RefreshResult, error) {
	strategy := o.Strategy
	if strategy == "" {
		strategy = p.Refresh
	}

	// Only rebase and merge can use it; recorded as what actually happens.
	autostash := o.Autostash && (strategy == config.StrategyRebase || strategy == config.StrategyMerge)

	result := &RefreshResult{
		Path:      p.Name,
		Dir:       p.Dir,
		Strategy:  strategy,
		Remote:    p.Remote,
		Branch:    p.Branch,
		Autostash: autostash,
	}

	if err := IsRepo(ctx, t, p.Dir); err != nil {
		return result, err
	}

	// A merge or rebase an earlier refresh left stopped is waiting for a
	// human. Starting another on top of it would at best fail and at worst
	// bury the conflict they need to look at.
	if op, _ := InProgress(ctx, t, p.Dir); op != "" {
		return result, ErrInProgress.New(
			"%s is stopped mid-%s; resolve it by hand (or abort it yourself) before refreshing again", p.Dir, op)
	}

	// Tracked changes only — see Status.
	status, err := Status(ctx, t, p.Dir, false)
	if err != nil {
		return result, err
	}

	result.WasDirty = status != ""
	if result.WasDirty {
		result.Stashed = len(strings.Split(status, "\n"))
	}

	if result.WasDirty && !o.Force && !p.AllowDirty && !o.Autostash {
		return result, ErrDirty.New(
			"%s has uncommitted changes to tracked files; refusing to refresh "+
				"(use autostash, pass --force, or set allow_dirty on the path):\n%s",
			p.Dir, status)
	}

	// Remember the top of the stash so that, afterwards, "a stash entry that
	// was not there before" identifies ours without parsing git's prose.
	stashBefore := ""
	if autostash && result.WasDirty {
		stashBefore, _ = stashTop(ctx, t, p.Dir)
	}

	if result.Before, err = Head(ctx, t, p.Dir); err != nil {
		return result, err
	}

	if err := Fetch(ctx, t, p.Dir, p.Remote); err != nil {
		return result, err
	}

	upstream := p.Remote + "/" + p.Branch

	var cmd string

	switch strategy {
	case config.StrategyFetchOnly:
		cmd = ""
	case config.StrategyFFOnly:
		cmd = git(p.Dir, "merge", "--ff-only", upstream)
	case config.StrategyRebase:
		if autostash {
			cmd = git(p.Dir, "rebase", "--autostash", upstream)
		} else {
			cmd = git(p.Dir, "rebase", upstream)
		}
	case config.StrategyMerge:
		if autostash {
			cmd = git(p.Dir, "merge", "--autostash", "--no-edit", upstream)
		} else {
			cmd = git(p.Dir, "merge", "--no-edit", upstream)
		}
	default:
		return result, ErrGit.New("unknown refresh strategy %q", strategy)
	}

	if cmd != "" {
		res, runErr := t.Run(ctx, cmd)
		if runErr != nil {
			return result, runErr
		}

		if autostash && result.WasDirty {
			result.StashRef = leftoverStash(ctx, t, p.Dir, stashBefore)
		}

		if !res.OK() {
			result.Message = res.Err()

			// A failed merge/rebase is left mid-operation on purpose: aborting
			// on the operator's behalf would throw away the conflict they need
			// to look at. Report what is conflicted and stop.
			conflicts, cErr := Conflicts(ctx, t, p.Dir)
			if cErr == nil && len(conflicts) > 0 {
				result.Conflict = true
				result.Conflicts = conflicts

				return result, ErrConflict.New("%s: %s left %d conflicted path(s) in %s — resolve or abort by hand",
					p.Name, strategy, len(conflicts), p.Dir)
			}

			return result, ErrGit.New("%s in %s failed (exit %d): %s", strategy, p.Dir, res.ExitCode, res.Err())
		}
	}

	if result.After, err = Head(ctx, t, p.Dir); err != nil {
		return result, err
	}

	result.Changed = result.Before != result.After

	// The operation "succeeded" but re-applying the stash conflicted: git keeps
	// the entry and the working tree holds conflict markers. Say so, because
	// exit 0 alone would read as a clean refresh.
	if result.StashRef != "" && result.Message == "" {
		result.Message = "refreshed, but re-applying your stashed changes conflicted; they are safe in stash " + result.StashRef
	}

	if result.Changed {
		res, dErr := t.Run(ctx, git(p.Dir, "diff", "--stat", result.Before, result.After))
		if dErr == nil && res.OK() {
			result.DiffStat = strings.TrimSpace(res.Stdout)
		}
	}

	return result, nil
}

// Conflicts lists paths left unmerged by a failed merge or rebase.
func Conflicts(ctx context.Context, t transport.Transport, dir string) ([]string, error) {
	res, err := t.Run(ctx, git(dir, "diff", "--name-only", "--diff-filter=U"))
	if err != nil {
		return nil, err
	}

	if !res.OK() {
		return nil, ErrGit.New("listing conflicts in %s failed: %s", dir, res.Err())
	}

	var out []string

	for _, line := range strings.Split(res.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}

	return out, nil
}

// AddWorktree creates (or reuses) a worktree at worktreeDir on branch,
// branched from remote/base.
//
// -B resets an existing branch to the base rather than failing, which is what
// makes relaunching a task id idempotent.
func AddWorktree(
	ctx context.Context,
	t transport.Transport,
	repoDir, worktreeDir, branch, remote, base string,
) error {
	if err := IsRepo(ctx, t, repoDir); err != nil {
		return err
	}

	if err := Fetch(ctx, t, repoDir, remote); err != nil {
		return err
	}

	// An existing worktree at this path means a previous run of the same task.
	// Reuse it rather than clobbering whatever work is in it.
	if exists, err := PathExists(ctx, t, worktreeDir); err != nil {
		return err
	} else if exists {
		return nil
	}

	cmd := fmt.Sprintf("mkdir -p %s && %s",
		util.ShellQuote(parentOf(worktreeDir)),
		git(repoDir, "worktree", "add", "-B", branch, worktreeDir, remote+"/"+base))

	res, err := t.Run(ctx, cmd)
	if err != nil {
		return err
	}

	if !res.OK() {
		return ErrGit.New("creating worktree %s (branch %s from %s/%s) failed (exit %d): %s",
			worktreeDir, branch, remote, base, res.ExitCode, res.Err())
	}

	return nil
}

// RemoveWorktree drops a worktree and prunes the administrative entry.
func RemoveWorktree(ctx context.Context, t transport.Transport, repoDir, worktreeDir string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, worktreeDir)

	res, err := t.Run(ctx, git(repoDir, args...))
	if err != nil {
		return err
	}

	if !res.OK() {
		return ErrGit.New("removing worktree %s failed (exit %d): %s", worktreeDir, res.ExitCode, res.Err())
	}

	if _, err := t.Run(ctx, git(repoDir, "worktree", "prune")); err != nil {
		return err
	}

	return nil
}

// RefExists reports whether ref resolves in dir. Read-only; a missing ref is
// (false, nil), reserving error for the transport itself failing.
func RefExists(ctx context.Context, t transport.Transport, dir, ref string) (bool, error) {
	res, err := t.Run(ctx, git(dir, "rev-parse", "--verify", "--quiet", ref))
	if err != nil {
		return false, err
	}

	return res.OK(), nil
}

// PathExists reports whether a path exists on the remote.
func PathExists(ctx context.Context, t transport.Transport, path string) (bool, error) {
	res, err := t.Run(ctx, "test -e "+util.ShellQuote(path))
	if err != nil {
		return false, err
	}

	return res.OK(), nil
}

func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "."
}

// RunGit runs a read-only git subcommand in dir and returns its stdout. A
// non-zero exit is an error carrying git's own message.
func RunGit(ctx context.Context, t transport.Transport, dir string, args ...string) (string, error) {
	res, err := t.Run(ctx, git(dir, args...))
	if err != nil {
		return "", err
	}

	if !res.OK() {
		return "", ErrGit.New("git %s in %s failed (exit %d): %s", strings.Join(args, " "), dir, res.ExitCode, res.Err())
	}

	return res.Stdout, nil
}

// InProgress reports a merge, rebase or cherry-pick that stopped and was left
// for a human: "merge", "rebase", "cherry-pick", or "" when there is none.
func InProgress(ctx context.Context, t transport.Transport, dir string) (string, error) {
	res, err := t.Run(ctx, "cd "+util.ShellQuote(dir)+" && "+inProgressProbe)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(res.Stdout), nil
}

// stashTop returns the commit at the top of the stash, "" when it is empty.
func stashTop(ctx context.Context, t transport.Transport, dir string) (string, error) {
	res, err := t.Run(ctx, git(dir, "rev-parse", "-q", "--verify", "refs/stash"))
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(res.Stdout), nil
}

// leftoverStash returns the stash commit that exists now but did not before,
// or "". When autostash succeeds git pops its entry and the stash returns to
// what it was; anything different afterwards is ours, kept because the
// operation stopped or re-applying it conflicted.
func leftoverStash(ctx context.Context, t transport.Transport, dir, before string) string {
	after, err := stashTop(ctx, t, dir)
	if err != nil || after == "" || after == before {
		return ""
	}

	return after
}
