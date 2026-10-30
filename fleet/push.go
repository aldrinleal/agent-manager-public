package fleet

import (
	"context"
	"fmt"
	"strings"

	"github.com/aldrinleal/agent-manager-public/gitx"
)

// PushPlan is what pushing a session's branch WOULD do.
type PushPlan struct {
	SessionID string `json:"session_id"`
	Task      string `json:"task_id"`
	Host      string `json:"host"`
	Worktree  string `json:"worktree"`
	Remote    string `json:"remote"`
	Branch    string `json:"branch"`

	// Commits are what would be sent: those on the branch that its upstream
	// (or, before the first push, the base) does not have. Newest first.
	Commits []string `json:"commits"`
	// Uncommitted are tracked changes that will NOT be pushed — a push sends
	// commits, and agents often leave work uncommitted.
	Uncommitted []string `json:"uncommitted"`

	// Blocked is true when Push would refuse; Reasons say why.
	Blocked  bool     `json:"blocked"`
	Reasons  []string `json:"reasons"`
	Warnings []string `json:"warnings"`
}

// PlanPush reports what Push would do. Read-only.
//
// Pushing is limited to the branch a session CREATED: agent/<task> in a
// worktree. It will not push an in-place session (whose branch is the repo's
// own, e.g. master), a worktree someone switched to another branch, or a
// checkout stopped mid-merge. Each is a case where "push" stops meaning "send
// the agent's work for review" and starts meaning "change something shared".
func (f *Fleet) PlanPush(ctx context.Context, ref string) (*PushPlan, error) {
	s, err := f.reg.Lookup(ctx, ref)
	if err != nil {
		return nil, err
	}

	target, err := f.ResolveTarget(s.Host, s.RepoName)
	if err != nil {
		return nil, err
	}

	p := &PushPlan{
		SessionID: s.ID, Task: s.TaskID, Host: s.Host, Worktree: s.Worktree,
		Remote: target.Path.Remote, Branch: s.Branch,
		Commits: []string{}, Uncommitted: []string{}, Reasons: []string{}, Warnings: []string{},
	}

	block := func(why string) { p.Blocked = true; p.Reasons = append(p.Reasons, why) }

	if s.Worktree == s.RepoDir {
		block(fmt.Sprintf("this is an in-place session on %q, the repo's own branch; push it by hand", s.Branch))
	}

	if !strings.HasPrefix(s.Branch, "agent/") {
		block(fmt.Sprintf("%q is not an agent/ branch; only branches a session created are pushed", s.Branch))
	}

	t, err := f.transportFor(s.Host)
	if err != nil {
		return nil, err
	}

	execCtx, cancel := f.ctxWithExecTimeout(ctx)
	defer cancel()

	st, err := gitx.GetRepoStatus(execCtx, t, s.Worktree)
	if err != nil {
		block("the worktree could not be read: " + err.Error())
		return p, nil
	}

	p.Uncommitted = st.Dirty

	switch {
	case st.InProgress != "":
		block(fmt.Sprintf("the worktree is stopped mid-%s; resolve it first", st.InProgress))
	case st.Branch == "":
		block("the worktree is on a detached HEAD")
	case st.Branch != s.Branch:
		block(fmt.Sprintf("the worktree is on %q, not the session's %q", st.Branch, s.Branch))
	}

	// What would travel: against the upstream if there is one, else the base.
	against := st.Upstream
	if against == "" {
		against = target.Path.Remote + "/" + target.Path.Branch
	}

	if out, lerr := gitx.RunGit(execCtx, t, s.Worktree, "log", "--oneline", against+"..HEAD"); lerr == nil {
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if l != "" {
				p.Commits = append(p.Commits, l)
			}
		}
	} else {
		p.Warnings = append(p.Warnings, "could not list the commits to push: "+lerr.Error())
	}

	if len(p.Commits) == 0 && !p.Blocked {
		p.Warnings = append(p.Warnings, "no commits beyond "+against+": there is nothing to push")
	}

	if len(p.Uncommitted) > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"%d uncommitted tracked file(s) will NOT be pushed; commit them first if they belong in the review", len(p.Uncommitted)))
	}

	if st.Behind > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"the branch is %d commit(s) behind %s; if the remote has moved the push will be rejected (never forced)", st.Behind, st.Upstream))
	}

	return p, nil
}

// Push sends a session's agent/<task> branch to its remote with -u and returns
// the remote's own output, including the pull-request link GitHub prints.
//
// It re-runs the plan's checks immediately before pushing rather than trusting
// that a confirmation was given for the same state, and it never forces.
func (f *Fleet) Push(ctx context.Context, ref string) (*gitx.PushResult, error) {
	plan, err := f.PlanPush(ctx, ref)
	if err != nil {
		return nil, err
	}

	if plan.Blocked {
		return nil, ErrConflict.New("not pushing %s: %s", plan.Branch, strings.Join(plan.Reasons, "; "))
	}

	t, err := f.transportFor(plan.Host)
	if err != nil {
		return nil, err
	}

	execCtx, cancel := context.WithTimeout(ctx, 6*f.cfg.ExecTimeout)
	defer cancel()

	return gitx.Push(execCtx, t, plan.Worktree, plan.Remote, plan.Branch, true)
}
