package fleet

import (
	"context"
	"strings"
	"testing"

	"github.com/aldrinleal/agent-manager-public/gitx"
	"github.com/aldrinleal/agent-manager-public/registry"
)

// readOnly is the ALLOWLIST of commands Plan may send. An allowlist, not a
// blocklist of write verbs: a new write added to Plan later fails this test
// without anyone having to remember to extend a list of bad words.
func readOnly(cmd string) bool {
	for _, ok := range []string{"test -e ", "'rev-parse'", "'status' '--porcelain'", "--porcelain=v2"} {
		if strings.Contains(cmd, ok) {
			return true
		}
	}

	return false
}

func planReq() LaunchRequest {
	return LaunchRequest{Host: "homelab", Path: "notes-svc", TaskID: "preview", Prompt: "Fix the bug.\nThen test."}
}

func TestPlanHasNoSideEffects(t *testing.T) {
	f, host := newTestFleet(t)
	ctx := context.Background()

	plan, err := f.Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}

	if plan.Blocked || plan.Conflict != nil {
		t.Fatalf("fresh plan should not conflict: %+v", plan)
	}

	for _, cmd := range host.Commands() {
		if !readOnly(cmd) {
			t.Errorf("Plan sent a non-read command to the host: %s", cmd)
		}
	}

	if len(host.files) != 0 {
		t.Errorf("Plan wrote files to the host: %v", host.files)
	}

	sessions, err := f.Registry().List(ctx, registry.Filter{})
	if err != nil || len(sessions) != 0 {
		t.Errorf("Plan created a session row: %v (err %v)", sessions, err)
	}

	// It reads the preflight cache but must not refresh it: that is a write.
	if c, _ := f.Registry().HostCheck(ctx, "homelab"); c != nil {
		t.Errorf("Plan wrote a host_checks row: %+v", c)
	}
}

// The property the whole feature rests on: what a human confirms is what runs.
func TestPlanAndLaunchAgree(t *testing.T) {
	f, _ := newTestFleet(t)
	ctx := context.Background()

	plan, err := f.Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}

	s, err := f.Launch(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}

	for name, pair := range map[string][2]string{
		"worktree": {plan.Worktree, s.Worktree},
		"branch":   {plan.Branch, s.Branch},
		"session":  {plan.Session, s.TmuxSession},
		"log":      {plan.LogPath, s.LogPath},
		"task":     {plan.TaskID, s.TaskID},
		"repo":     {plan.RepoDir, s.RepoDir},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: plan said %q, launch did %q", name, pair[0], pair[1])
		}
	}
}

func TestPlanReportsAnActiveConflictAsBlocked(t *testing.T) {
	f, _ := newTestFleet(t)
	ctx := context.Background()

	first, err := f.Launch(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}

	plan, err := f.Plan(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}

	if !plan.Blocked || plan.Conflict == nil || plan.Conflict.ID != first.ID {
		t.Fatalf("expected a blocking conflict with %s, got %+v", first.ID, plan)
	}

	// With force it is no longer blocked, but it must say it will replace.
	req := planReq()
	req.Force = true

	plan, err = f.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Blocked || !plan.WillReplace {
		t.Errorf("forced plan should replace, not block: blocked=%v replace=%v", plan.Blocked, plan.WillReplace)
	}
}

func TestPlanWarnsWhenTheWorktreeWillBeReused(t *testing.T) {
	f, host := newTestFleet(t)
	host.worktreeSeen = true

	plan, err := f.Plan(context.Background(), planReq())
	if err != nil {
		t.Fatal(err)
	}

	if !plan.WorktreeExists {
		t.Fatal("worktree should be reported as existing")
	}

	if !strings.Contains(strings.Join(plan.Warnings, "|"), "REUSED") {
		t.Errorf("no reuse warning: %v", plan.Warnings)
	}
}

func TestPlanReportsDirtyTrackedFilesInAReusedCheckout(t *testing.T) {
	f, host := newTestFleet(t)
	host.worktreeSeen = true
	host.dirty = " M app/tax.go\n M app/vat.go\n"

	plan, err := f.Plan(context.Background(), planReq())
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Dirty) != 2 {
		t.Fatalf("dirty = %v, want 2 entries", plan.Dirty)
	}
}

func TestPlanWarnsAboutAnUnpreflightedHost(t *testing.T) {
	f, _ := newTestFleet(t)

	plan, err := f.Plan(context.Background(), planReq())
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(strings.Join(plan.Warnings, "|"), "not been preflighted") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
}

func TestPlanInplaceNamesNoNewBranch(t *testing.T) {
	f, _ := newTestFleet(t)

	plan, err := f.Plan(context.Background(), LaunchRequest{
		Host: "homelab", Path: "cli-kit", TaskID: "lint", Prompt: "go",
	})
	if err != nil {
		t.Fatal(err)
	}

	if plan.Branch != plan.Base {
		t.Errorf("in-place creates no branch: base=%q branch=%q", plan.Base, plan.Branch)
	}

	if !strings.Contains(strings.Join(plan.Warnings, "|"), "in-place") {
		t.Errorf("in-place should be called out: %v", plan.Warnings)
	}
}

// Plan validates exactly as Launch does, so a bad request fails at the
// preview and never reaches a confirmation prompt.
func TestPlanRejectsWhatLaunchRejects(t *testing.T) {
	f, _ := newTestFleet(t)

	for name, req := range map[string]LaunchRequest{
		"empty prompt":  {Host: "homelab", Path: "notes-svc", TaskID: "t", Prompt: "  "},
		"unlisted path": {Host: "homelab", Path: "/etc", TaskID: "t", Prompt: "x"},
		"jump host":     {Host: "bastion", Path: "x", TaskID: "t", Prompt: "x"},
		"no task id":    {Host: "homelab", Path: "notes-svc", TaskID: "!!!", Prompt: "x"},
	} {
		if _, err := f.Plan(context.Background(), req); err == nil {
			t.Errorf("%s: Plan accepted it", name)
		}
	}
}

func TestPlanRefreshIsReadOnlyAndReportsDirty(t *testing.T) {
	f, host := newTestFleet(t)
	host.dirty = " M app/tax.go\n"

	items, err := f.PlanRefresh(context.Background(), "homelab", "notes-svc", gitx.RefreshOpts{})
	if err != nil {
		t.Fatal(err)
	}

	if len(items) != 1 || !items[0].WouldBlock || len(items[0].Dirty) != 1 {
		t.Fatalf("want one blocked item with one dirty file, got %+v", items)
	}

	for _, cmd := range host.Commands() {
		if !readOnly(cmd) {
			t.Errorf("PlanRefresh sent a non-read command: %s", cmd)
		}
	}

	// force lifts the block, and allow_dirty-style overrides must be visible.
	items, _ = f.PlanRefresh(context.Background(), "homelab", "notes-svc", gitx.RefreshOpts{Force: true})
	if items[0].WouldBlock {
		t.Error("force should lift the block")
	}
}

func TestPlanRefreshWholeHostCoversEveryPath(t *testing.T) {
	f, _ := newTestFleet(t)

	items, err := f.PlanRefresh(context.Background(), "homelab", "", gitx.RefreshOpts{Strategy: "rebase"})
	if err != nil {
		t.Fatal(err)
	}

	if len(items) != 2 {
		t.Fatalf("homelab has two paths, plan has %d", len(items))
	}

	for _, it := range items {
		if it.Strategy != "rebase" {
			t.Errorf("%s: strategy %q, want the requested rebase", it.Path, it.Strategy)
		}

		if !strings.Contains(strings.Join(it.Warnings, "|"), "mid-operation") {
			t.Errorf("%s: rebase must warn about conflicts: %v", it.Path, it.Warnings)
		}
	}
}

func TestPlanRefreshAutostashSaysWhatWillBeStashed(t *testing.T) {
	f, host := newTestFleet(t)
	host.dirty = " M a.go\n M b.go\n M c.go\n"

	items, err := f.PlanRefresh(context.Background(), "homelab", "notes-svc", gitx.RefreshOpts{Strategy: "rebase", Autostash: true})
	if err != nil {
		t.Fatal(err)
	}

	it := items[0]

	if it.WouldBlock || !it.WillStash || len(it.Dirty) != 3 {
		t.Fatalf("autostash+rebase on 3 dirty files should proceed and stash: %+v", it)
	}

	// The handover's wording requirement: the plan must say how many.
	if !strings.Contains(strings.Join(it.Warnings, "|"), "3 dirty file(s) will be stashed and re-applied") {
		t.Errorf("warnings = %v", it.Warnings)
	}
}

func TestPlanRefreshFFOnlyAutostashStashesNothing(t *testing.T) {
	f, host := newTestFleet(t)
	host.dirty = " M a.go\n"

	items, _ := f.PlanRefresh(context.Background(), "homelab", "notes-svc", gitx.RefreshOpts{Strategy: "ff-only", Autostash: true})

	if items[0].WillStash || items[0].WouldBlock {
		t.Errorf("ff-only never stashes and is safe on a dirty tree: %+v", items[0])
	}
}

func TestPlanRefreshStoppedMergeBlocksEvenWithAutostash(t *testing.T) {
	f, host := newTestFleet(t)
	host.statusOut = "# branch.oid abc\n# branch.head master\n@@INPROGRESS\nmerge\n"

	items, _ := f.PlanRefresh(context.Background(), "homelab", "notes-svc", gitx.RefreshOpts{Autostash: true, Force: true})

	if !items[0].WouldBlock || items[0].InProgress != "merge" {
		t.Fatalf("a stopped merge must block regardless of flags: %+v", items[0])
	}

	if !strings.Contains(strings.Join(items[0].Warnings, "|"), "mid-merge") {
		t.Errorf("warnings = %v", items[0].Warnings)
	}
}
