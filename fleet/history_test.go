package fleet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aldrinleal/agent-manager-public/transport"
)

var hseq int64

// hline builds one history.jsonl line. The timestamp gets a strictly increasing
// offset: two prompts in one session never share a millisecond in real data, and
// the store's key is (host, session, ts), so identical fixtures would collapse.
func hline(session, project, text string, daysAgo int) string {
	hseq++
	ts := time.Now().AddDate(0, 0, -daysAgo).UnixMilli() + hseq

	return fmt.Sprintf(`{"display":%q,"pastedContents":{},"timestamp":%d,"project":%q,"sessionId":%q}`, text, ts, project, session)
}

func historyFile(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

func TestPullHistoryIsIdempotent(t *testing.T) {
	f, host := newTestFleet(t)
	host.historyOut = historyFile(
		hline("s1", "/s/api-kit", "fix the tax", 3),
		hline("s1", "/s/api-kit", "add a test", 3),
		hline("s2", "/s/cli-kit", "lint", 5),
	)

	first, err := f.PullHistory(context.Background(), "homelab")
	if err != nil || first.Inserted != 3 || first.Parsed != 3 {
		t.Fatalf("first pull: %+v %v", first, err)
	}

	// The acceptance check: re-reading the same file adds nothing.
	second, err := f.PullHistory(context.Background(), "homelab")
	if err != nil || second.Inserted != 0 || second.Parsed != 3 {
		t.Fatalf("second pull must insert 0, got %+v %v", second, err)
	}

	// And new lines are picked up on the next pull.
	host.historyOut += hline("s3", "/s/api-kit", "later", 1) + "\n"

	third, _ := f.PullHistory(context.Background(), "homelab")
	if third.Inserted != 1 {
		t.Errorf("one new line should insert 1, got %+v", third)
	}
}

func TestPullHistoryRecordsAMissingFileWithoutFailing(t *testing.T) {
	f, host := newTestFleet(t)
	host.historyMissing = true

	p, err := f.PullHistory(context.Background(), "homelab")
	if err != nil {
		t.Fatalf("a host with no history must not be an error: %v", err)
	}

	if p.Inserted != 0 || !strings.Contains(p.Note, "No such file") {
		t.Errorf("%+v", p)
	}

	pulls, _ := f.HistoryPulls(context.Background())
	if len(pulls) != 1 || pulls[0].Host != "homelab" || pulls[0].Note == "" {
		t.Errorf("the outcome must be recorded: %+v", pulls)
	}
}

func TestPullHistoryNotesUnparseableLines(t *testing.T) {
	f, host := newTestFleet(t)
	host.historyOut = historyFile(hline("s1", "/s/a", "ok", 1), "garbage line", `{"half": `)

	p, _ := f.PullHistory(context.Background(), "homelab")

	if p.Inserted != 1 || p.Bad != 2 || !strings.Contains(p.Note, "2 line(s)") {
		t.Errorf("%+v", p)
	}
}

// One unreachable host (the laptop is asleep) must not stop the others.
func TestPullHistoryAllSurvivesAnUnreachableHost(t *testing.T) {
	f, host := newTestFleet(t)
	host.historyOut = historyFile(hline("s1", "/s/a", "x", 1))

	good := f.transports
	f.transports = func(name string) (transport.Transport, error) {
		if name == "server" {
			return nil, fmt.Errorf("dial tcp: i/o timeout")
		}

		return good(name)
	}

	got := f.PullHistoryAll(context.Background())

	byHost := map[string]string{}
	for _, p := range got {
		byHost[p.Host] = fmt.Sprintf("%d|%s", p.Inserted, p.Note)
	}

	if !strings.HasPrefix(byHost["homelab"], "1|") {
		t.Errorf("the reachable host must still be read: %v", byHost)
	}

	if !strings.Contains(byHost["server"], "unreachable") {
		t.Errorf("the dead host must be noted, not fatal: %v", byHost)
	}

	// A jump host has no claude and is skipped entirely.
	if _, ok := byHost["bastion"]; ok {
		t.Error("a jump-only host must not be read")
	}
}

func TestStorePromptsOffKeepsTheEntryButNotTheText(t *testing.T) {
	f, host := newTestFleet(t)
	f.cfg.HistoryStorePrompts = false
	host.historyOut = historyFile(hline("s1", "/s/a", "a private thought", 1))

	if p, _ := f.PullHistory(context.Background(), "homelab"); p.Inserted != 1 {
		t.Fatalf("%+v", p)
	}

	days, err := f.HistoryWorklog(context.Background(), "", 7, "UTC")
	if err != nil || len(days) != 1 {
		t.Fatalf("%v %+v", err, days)
	}

	s := days[0].Projects[0].Sessions[0]
	if s.PromptCount != 1 || len(s.Prompts) != 0 {
		t.Errorf("counts only when prompts are not stored: %+v", s)
	}
}

func TestWorklogHonoursHostExcludesAndTimeZone(t *testing.T) {
	f, host := newTestFleet(t)
	host.historyOut = historyFile(
		hline("s1", "/s/api-kit", "visible", 0),
		hline("s2", "/home/dev/Course-SCRATCH-x", "excluded", 0),
	)

	_, _ = f.PullHistory(context.Background(), "homelab")

	days, err := f.HistoryWorklog(context.Background(), "homelab", 3, "America/Bogota")
	if err != nil || len(days) == 0 {
		t.Fatalf("%v %+v", err, days)
	}

	for _, d := range days {
		for _, p := range d.Projects {
			if strings.Contains(strings.ToLower(p.Project), "scratch") {
				t.Errorf("an excluded project leaked into the work log: %s", p.Project)
			}
		}
	}

	if _, err := f.HistoryWorklog(context.Background(), "", 3, "Not/AZone"); err == nil {
		t.Error("an unknown time zone must be an error")
	}

	if _, err := f.HistoryWorklog(context.Background(), "nosuchhost", 3, "UTC"); err == nil {
		t.Error("an unknown host must be an error")
	}
}

// End to end through the fleet: ingest, resolve over the (fake) host, propose.
func TestProposalsEndToEnd(t *testing.T) {
	f, host := newTestFleet(t)

	var lines []string
	for i := 0; i < 5; i++ {
		lines = append(lines, hline(fmt.Sprintf("m%d", i), "/home/dev/src/api-kit2", "work", 2))
		lines = append(lines, hline(fmt.Sprintf("h%d", i), "/home/dev/src/flowlab", "work", 2))
		lines = append(lines, hline(fmt.Sprintf("c%d", i), "/home/dev/src/notes-svc", "work", 2)) // already allowlisted
	}

	host.historyOut = historyFile(lines...)
	host.resolveOut = strings.Join([]string{
		"@@P|/home/dev/src/api-kit2|OK|/home/dev/src/api-kit2|master|1",
		"@@P|/home/dev/src/flowlab|OK|/home/dev/src/flowlab||0",
		"@@P|/home/dev/src/notes-svc|OK|/home/dev/src/notes-svc|main|1",
	}, "\n") + "\n"

	if _, err := f.PullHistory(context.Background(), "homelab"); err != nil {
		t.Fatal(err)
	}

	res, err := f.HistoryProposals(context.Background(), 90)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, p := range res.Proposals {
		got[p.Project[strings.LastIndex(p.Project, "/")+1:]] = p.Verdict + ":" + p.Reason
	}

	if !strings.HasPrefix(got["api-kit2"], "propose") {
		t.Errorf("api-kit2: %v", got)
	}

	if !strings.Contains(got["flowlab"], "empty repo") {
		t.Errorf("flowlab must be reported as an empty repo: %v", got)
	}

	if !strings.HasPrefix(got["notes-svc"], "allowlisted") {
		t.Errorf("notes-svc is in the allowlist: %v", got)
	}

	if !strings.Contains(res.YAML, "- name: api-kit2") || strings.Contains(res.YAML, "flowlab") {
		t.Errorf("the YAML must contain only what is proposed:\n%s", res.YAML)
	}
}

// Observers are read from but never proposed for: only launch targets get a
// resolution, so a project that exists only on a server cannot be proposed.
func TestProposalsNeverTargetAnObserver(t *testing.T) {
	f, host := newTestFleet(t)
	host.historyOut = historyFile(hline("s1", "/s/serveronly", "x", 1))
	host.resolveOut = "@@P|/s/serveronly|OK|/s/serveronly|main|1\n"

	_, _ = f.PullHistory(context.Background(), "server") // the observer

	res, _ := f.HistoryProposals(context.Background(), 90)

	if strings.Contains(res.YAML, "server") {
		t.Errorf("a proposal named the observer host:\n%s", res.YAML)
	}

	for _, p := range res.Proposals {
		for _, pp := range p.Paths {
			if pp.Host == "server" || pp.Host == "bastion" {
				t.Errorf("proposed a path on %s", pp.Host)
			}
		}
	}
}

// A session launched by agent-manager runs in <worktree_root>/<repo>/<task>.
// That is work ON the repo, not a project of its own.
func TestWorktreeSessionsAreAttributedToTheirRepo(t *testing.T) {
	f, host := newTestFleet(t)

	host.historyOut = historyFile(
		hline("s1", "/home/dev/src/notes-svc", "by hand", 2),
		hline("s2", "/srv/agent-workspaces/notes-svc/fix-tax", "launched by the manager", 2),
		hline("s3", "/srv/agent-workspaces/notes-svc/another-task", "launched again", 1),
		hline("s4", "/srv/agent-workspaces/gone-repo/x", "repo no longer in the inventory", 1),
	)

	if _, err := f.PullHistory(context.Background(), "homelab"); err != nil {
		t.Fatal(err)
	}

	pv, err := f.HistoryProjects(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}

	byProject := map[string]int{}
	for _, p := range pv {
		byProject[p.Project] = p.Sessions
	}

	if byProject["/home/dev/src/notes-svc"] != 3 {
		t.Errorf("all three sessions are work on notes-svc: %v", byProject)
	}

	for p := range byProject {
		if strings.Contains(p, "fix-tax") || strings.Contains(p, "another-task") {
			t.Errorf("a worktree leaked through as a project of its own: %s", p)
		}
	}

	// An unknown repo is left alone rather than guessed.
	if byProject["/srv/agent-workspaces/gone-repo/x"] != 1 {
		t.Errorf("a worktree of an unknown repo must be left as is: %v", byProject)
	}
}
