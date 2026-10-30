package fleet

import (
	"context"
	"strings"
	"time"

	"github.com/aldrinleal/agent-manager-public/gitx"
)

// statusTTL is how long a host's path-status sweep is reused. Long enough that
// a console polling every few seconds does not put an SSH round trip on every
// host per request; short enough that "is it dirty?" is never stale for long.
const statusTTL = 60 * time.Second

type statusEntry struct {
	at      time.Time
	entries []PathStatus
}

// PathStatus is one allowlisted checkout and what state it is in.
type PathStatus struct {
	Host      string           `json:"host"`
	Path      string           `json:"path"`
	Dir       string           `json:"dir"`
	Status    *gitx.RepoStatus `json:"status,omitempty"`
	Error     string           `json:"error,omitempty"`
	CheckedAt time.Time        `json:"checked_at"`
	Cached    bool             `json:"cached"`
}

// PathsStatus reports every allowlisted path — optionally narrowed to one host
// (and one path) — with its RepoStatus.
//
// One round trip PER HOST, not per path: a single compound command runs the
// status for every path on that host (gitx.StatusSweepCommand), the same trick
// as the reconcile sweep. Results are cached per host for statusTTL; fresh
// bypasses the cache. An unreachable host yields an error on each of its
// paths and never fails the whole call.
func (f *Fleet) PathsStatus(ctx context.Context, hostName, pathName string, fresh bool) ([]PathStatus, error) {
	var out []PathStatus

	for i := range f.inv.Hosts {
		h := &f.inv.Hosts[i]

		if !h.IsLaunchTarget() || (hostName != "" && h.Name != hostName) {
			continue
		}

		if hostName != "" && pathName != "" {
			if _, err := h.Path(pathName); err != nil {
				return nil, err
			}
		}

		entries := f.sweepHost(ctx, h.Name, fresh)

		for _, e := range entries {
			if pathName == "" || e.Path == pathName {
				out = append(out, e)
			}
		}
	}

	if hostName != "" && len(out) == 0 {
		if _, err := f.inv.Host(hostName); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (f *Fleet) sweepHost(ctx context.Context, hostName string, fresh bool) []PathStatus {
	f.statusMu.Lock()
	cached, ok := f.statusCache[hostName]
	f.statusMu.Unlock()

	if ok && !fresh && time.Since(cached.at) < statusTTL {
		out := make([]PathStatus, len(cached.entries))
		for i, e := range cached.entries {
			e.Cached = true
			out[i] = e
		}

		return out
	}

	host, _ := f.inv.Host(hostName)
	now := time.Now().UTC()

	paths := make([]gitx.SweepPath, 0, len(host.Paths))
	byName := make(map[string]PathStatus, len(host.Paths))

	for _, p := range host.Paths {
		paths = append(paths, gitx.SweepPath{Name: p.Name, Dir: p.Dir})
		byName[p.Name] = PathStatus{Host: hostName, Path: p.Name, Dir: p.Dir, CheckedAt: now}
	}

	fail := func(msg string) []PathStatus {
		res := make([]PathStatus, 0, len(host.Paths))

		for _, p := range host.Paths {
			e := byName[p.Name]
			e.Error = msg
			res = append(res, e)
		}

		return res
	}

	t, err := f.transportFor(hostName)
	if err != nil {
		return fail("host unreachable: " + err.Error())
	}

	// A sweep over a big repo can take a while; give it the long timeout.
	execCtx, cancel := context.WithTimeout(ctx, 3*f.cfg.ExecTimeout)
	defer cancel()

	res, err := t.Run(execCtx, gitx.StatusSweepCommand(paths))
	if err != nil {
		return fail("status sweep failed: " + err.Error())
	}

	for _, e := range gitx.ParseStatusSweep(res.Stdout) {
		ps, known := byName[e.Name]
		if !known {
			continue
		}

		ps.Status, ps.Error = e.Status, e.Err
		byName[e.Name] = ps
	}

	result := make([]PathStatus, 0, len(host.Paths))

	for _, p := range host.Paths {
		e := byName[p.Name]
		if e.Status == nil && e.Error == "" {
			e.Error = "no result for this path in the sweep output"
		}

		result = append(result, e)
	}

	f.statusMu.Lock()
	f.statusCache[hostName] = statusEntry{at: now, entries: result}
	f.statusMu.Unlock()

	return result
}

// SessionDiff is what an agent has done in its worktree, for a human to read
// before anything is pushed.
type SessionDiff struct {
	SessionID string           `json:"session_id"`
	Task      string           `json:"task_id"`
	Worktree  string           `json:"worktree"`
	Base      string           `json:"base"` // e.g. origin/master
	Status    *gitx.RepoStatus `json:"status"`

	// Commits are the commits on the worktree's branch not on Base, newest first.
	Commits []string `json:"commits"`
	// DiffStat compares the WORKING TREE to the fork point, so it covers both
	// commits and uncommitted tracked changes.
	DiffStat string `json:"diffstat"`

	Warnings []string `json:"warnings"`
}

// Diff reports the state and the work of one session's worktree. Read-only.
func (f *Fleet) Diff(ctx context.Context, ref string) (*SessionDiff, error) {
	s, err := f.reg.Lookup(ctx, ref)
	if err != nil {
		return nil, err
	}

	t, err := f.transportFor(s.Host)
	if err != nil {
		return nil, err
	}

	execCtx, cancel := context.WithTimeout(ctx, 3*f.cfg.ExecTimeout)
	defer cancel()

	d := &SessionDiff{
		SessionID: s.ID, Task: s.TaskID, Worktree: s.Worktree,
		Commits: []string{}, Warnings: []string{},
	}

	st, err := gitx.GetRepoStatus(execCtx, t, s.Worktree)
	if err != nil {
		return nil, err
	}

	d.Status = st

	// The base comes from the inventory. If the path has since been removed
	// from it, say so rather than guessing a branch name.
	target, rerr := f.ResolveTarget(s.Host, s.RepoName)
	if rerr != nil {
		d.Warnings = append(d.Warnings, "path is no longer in the inventory, so no base to compare against: "+rerr.Error())
		return d, nil
	}

	d.Base = target.Path.Remote + "/" + target.Path.Branch

	if s.Worktree == s.RepoDir {
		d.Warnings = append(d.Warnings, "in-place session: the checkout is the repo itself, so this includes anything already there")
	}

	if ok, _ := gitx.RefExists(execCtx, t, s.Worktree, d.Base); !ok {
		d.Warnings = append(d.Warnings, d.Base+" does not resolve in this worktree; run a refresh/fetch first")
		return d, nil
	}

	if log, lerr := gitx.RunGit(execCtx, t, s.Worktree, "log", "--oneline", d.Base+"..HEAD"); lerr == nil {
		for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
			if l != "" {
				d.Commits = append(d.Commits, l)
			}
		}
	}

	if mb, merr := gitx.RunGit(execCtx, t, s.Worktree, "merge-base", "HEAD", d.Base); merr == nil {
		d.DiffStat, _ = gitx.RunGit(execCtx, t, s.Worktree, "diff", "--stat", strings.TrimSpace(mb))
		d.DiffStat = strings.TrimSpace(d.DiffStat)
	}

	return d, nil
}
