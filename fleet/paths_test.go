package fleet

import (
	"context"
	"github.com/aldrinleal/agent-manager-public/transport"
	"strings"
	"testing"
)

const sweepSample = "@@PATH notes-svc\n# branch.oid aaaa\n# branch.head master\n# branch.upstream origin/master\n# branch.ab +1 -0\n" +
	"1 .M N... 100644 100644 100644 h h app/tax.go\n? junk.txt\n@@INPROGRESS\n@@END\n" +
	"@@PATH cli-kit\n@@ERR missing or not a git repository: /home/dev/src/cli-kit\n@@END\n"

func countSweeps(cmds []string) int {
	n := 0

	for _, c := range cmds {
		if strings.Contains(c, "@@PATH ") {
			n++
		}
	}

	return n
}

func TestPathsStatusIsOneRoundTripPerHost(t *testing.T) {
	f, host := newTestFleet(t)
	host.sweepOut = sweepSample

	got, err := f.PathsStatus(context.Background(), "", "", true)
	if err != nil {
		t.Fatal(err)
	}

	// homelab has two paths; bastion is jump-only and must be skipped.
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}

	if n := countSweeps(host.Commands()); n != 1 {
		t.Errorf("%d sweeps for one host with two paths, want exactly 1", n)
	}
}

func TestPathsStatusOneBadPathDoesNotPoisonTheRest(t *testing.T) {
	f, host := newTestFleet(t)
	host.sweepOut = sweepSample

	got, _ := f.PathsStatus(context.Background(), "homelab", "", true)

	byName := map[string]PathStatus{}
	for _, e := range got {
		byName[e.Path] = e
	}

	if s := byName["notes-svc"].Status; s == nil || len(s.Dirty) != 1 || s.Ahead != 1 || s.Untracked != 1 {
		t.Errorf("notes-svc: %+v", byName["notes-svc"])
	}

	if e := byName["cli-kit"]; e.Status != nil || !strings.Contains(e.Error, "not a git repository") {
		t.Errorf("cli-kit should carry its own error: %+v", e)
	}
}

func TestPathsStatusCachesAndFreshBypasses(t *testing.T) {
	f, host := newTestFleet(t)
	host.sweepOut = sweepSample
	ctx := context.Background()

	if _, err := f.PathsStatus(ctx, "homelab", "", false); err != nil {
		t.Fatal(err)
	}

	second, _ := f.PathsStatus(ctx, "homelab", "", false)

	if n := countSweeps(host.Commands()); n != 1 {
		t.Errorf("second call within the TTL should be cached, saw %d sweeps", n)
	}

	for _, e := range second {
		if !e.Cached {
			t.Errorf("%s should be marked cached", e.Path)
		}
	}

	if _, err := f.PathsStatus(ctx, "homelab", "", true); err != nil {
		t.Fatal(err)
	}

	if n := countSweeps(host.Commands()); n != 2 {
		t.Errorf("fresh=true must bypass the cache, saw %d sweeps", n)
	}
}

func TestPathsStatusUnreachableHostIsPerPathErrorsNotAFailure(t *testing.T) {
	f, _ := newTestFleet(t)

	f.transports = func(string) (transport.Transport, error) { return nil, context.DeadlineExceeded }

	got, err := f.PathsStatus(context.Background(), "homelab", "", true)
	if err != nil {
		t.Fatalf("an unreachable host must not fail the call: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d", len(got))
	}

	for _, e := range got {
		if e.Status != nil || !strings.Contains(e.Error, "unreachable") {
			t.Errorf("%s: %+v", e.Path, e)
		}
	}
}

func TestPathsStatusRejectsUnknownHostAndPath(t *testing.T) {
	f, _ := newTestFleet(t)

	if _, err := f.PathsStatus(context.Background(), "nosuch", "", false); err == nil {
		t.Error("unknown host should be an error")
	}

	if _, err := f.PathsStatus(context.Background(), "homelab", "not-listed", false); err == nil {
		t.Error("a path outside the allowlist should be an error")
	}
}

func TestDiffReportsWhatTheAgentDid(t *testing.T) {
	f, host := newTestFleet(t)
	ctx := context.Background()

	s, err := f.Launch(ctx, planReq())
	if err != nil {
		t.Fatal(err)
	}

	host.statusOut = "# branch.oid cc\n# branch.head agent/preview\n# branch.ab +2 -0\n1 .M N... 100644 100644 100644 h h app/tax.go\n@@INPROGRESS\n"
	host.logOut = "abc1234 Round the tax\ndef5678 Add a test\n"
	host.mergeBase = "0123456\n"

	d, err := f.Diff(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}

	if d.Base != "origin/main" {
		t.Errorf("base = %q", d.Base)
	}

	if len(d.Commits) != 2 || d.Commits[0] != "abc1234 Round the tax" {
		t.Errorf("commits = %v", d.Commits)
	}

	if d.Status == nil || d.Status.Ahead != 2 || len(d.Status.Dirty) != 1 {
		t.Errorf("status = %+v", d.Status)
	}

	if !strings.Contains(d.DiffStat, "1 file changed") {
		t.Errorf("diffstat = %q", d.DiffStat)
	}
}

func TestDiffWarnsWhenTheBaseDoesNotResolve(t *testing.T) {
	f, host := newTestFleet(t)
	ctx := context.Background()

	s, _ := f.Launch(ctx, planReq())

	host.statusOut = "# branch.oid cc\n# branch.head x\n@@INPROGRESS\n"
	host.refMissing = true

	d, err := f.Diff(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(strings.Join(d.Warnings, "|"), "does not resolve") {
		t.Errorf("warnings = %v", d.Warnings)
	}
}
