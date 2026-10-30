package fleet

import (
	"context"
	"strings"
	"testing"
)

const goodStatus = "# branch.oid cc\n# branch.head agent/preview\n# branch.upstream origin/main\n# branch.ab +2 -0\n@@INPROGRESS\n"

func pushed(host *fakeHost) bool {
	for _, c := range host.Commands() {
		if strings.Contains(c, "'push'") {
			return true
		}
	}

	return false
}

func launched(t *testing.T, f *Fleet, host *fakeHost) string {
	t.Helper()

	s, err := f.Launch(context.Background(), planReq())
	if err != nil {
		t.Fatal(err)
	}

	host.statusOut = goodStatus
	host.logOut = "abc1234 Round the tax\ndef5678 Add a test\n"

	return s.ID
}

func TestPlanPushListsWhatWillTravelAndWhatWillNot(t *testing.T) {
	f, host := newTestFleet(t)
	id := launched(t, f, host)

	host.statusOut = "# branch.oid cc\n# branch.head agent/preview\n# branch.upstream origin/main\n# branch.ab +2 -0\n" +
		"1 .M N... 100644 100644 100644 h h left/alone.go\n@@INPROGRESS\n"

	plan, err := f.PlanPush(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Blocked {
		t.Fatalf("a clean agent branch should be pushable: %+v", plan)
	}

	if plan.Branch != "agent/preview" || len(plan.Commits) != 2 {
		t.Errorf("plan = %+v", plan)
	}

	if len(plan.Uncommitted) != 1 || !strings.Contains(strings.Join(plan.Warnings, "|"), "will NOT be pushed") {
		t.Errorf("uncommitted work must be called out: %+v", plan)
	}

	if pushed(host) {
		t.Error("planning a push must not push")
	}
}

func TestPushSendsTheBranchAndReturnsTheReviewLink(t *testing.T) {
	f, host := newTestFleet(t)
	id := launched(t, f, host)

	res, err := f.Push(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	if res.PRURL != "https://github.com/o/r/pull/new/agent/preview" {
		t.Errorf("pr url = %q", res.PRURL)
	}

	var cmd string

	for _, c := range host.Commands() {
		if strings.Contains(c, "'push'") {
			cmd = c
		}
	}

	for _, bad := range []string{"--force", "'-f'", "--mirror", "--delete"} {
		if strings.Contains(cmd, bad) {
			t.Errorf("push command contains %s: %s", bad, cmd)
		}
	}

	if !strings.Contains(cmd, "'-u'") {
		t.Errorf("first push should set the upstream: %s", cmd)
	}
}

func TestPushRefusesWhatIsNotAnAgentBranchInAWorktree(t *testing.T) {
	cases := map[string]struct {
		status string
		reason string
	}{
		"switched branch": {"# branch.oid c\n# branch.head master\n@@INPROGRESS\n", "not the session's"},
		"detached":        {"# branch.oid c\n# branch.head (detached)\n@@INPROGRESS\n", "detached"},
		"stopped merge":   {"# branch.oid c\n# branch.head agent/preview\n@@INPROGRESS\nmerge\n", "mid-merge"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, host := newTestFleet(t)
			id := launched(t, f, host)
			host.statusOut = c.status

			plan, err := f.PlanPush(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}

			if !plan.Blocked || !strings.Contains(strings.Join(plan.Reasons, "|"), c.reason) {
				t.Fatalf("want blocked with %q, got %+v", c.reason, plan)
			}

			if _, err := f.Push(context.Background(), id); err == nil {
				t.Error("Push must refuse what the plan blocks")
			}

			if pushed(host) {
				t.Error("a refused push reached the host")
			}
		})
	}
}

// An in-place session works on the repo's own branch; pushing it is changing
// something shared, not sending work for review.
func TestPushRefusesAnInplaceSession(t *testing.T) {
	f, host := newTestFleet(t)

	s, err := f.Launch(context.Background(), LaunchRequest{Host: "homelab", Path: "cli-kit", TaskID: "lint", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}

	host.statusOut = "# branch.oid c\n# branch.head main\n@@INPROGRESS\n"

	plan, _ := f.PlanPush(context.Background(), s.ID)
	if !plan.Blocked || !strings.Contains(strings.Join(plan.Reasons, "|"), "in-place") {
		t.Fatalf("%+v", plan)
	}

	if _, err := f.Push(context.Background(), s.ID); err == nil || pushed(host) {
		t.Error("an in-place push must be refused before reaching the host")
	}
}

func TestPushSurfacesARejectionAndNeverRetriesWithForce(t *testing.T) {
	f, host := newTestFleet(t)
	id := launched(t, f, host)
	host.pushFail = true

	_, err := f.Push(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "non-fast-forward") {
		t.Fatalf("the remote's refusal should come back verbatim, got %v", err)
	}

	n := 0

	for _, c := range host.Commands() {
		if strings.Contains(c, "'push'") {
			n++
		}
	}

	if n != 1 {
		t.Errorf("a rejected push must not be retried; saw %d push commands", n)
	}
}
