package fleet

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/tmux"
)

func realPrompt(t *testing.T) string {
	t.Helper()

	b, err := os.ReadFile("../runner/testdata/permission-prompt.txt")
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func blockedSession(state registry.State, age time.Duration, now time.Time) *registry.Session {
	return &registry.Session{
		TmuxSession: "s", LogPath: "/log", State: state, Runner: "claude-code",
		CreatedAt: now.Add(-age), RemoteURL: "u",
	}
}

func TestBlockedBeatsRunningStaleAndStarting(t *testing.T) {
	now := time.Date(2026, 10, 28, 22, 0, 0, 0, time.UTC)
	panes := map[string]tmux.Pane{"s": {Session: "s", PID: 9}}
	screens := map[string]string{"s": realPrompt(t)}
	quiet := map[string]logStat{"/log": {ModTime: now.Add(-3 * time.Hour)}}

	for name, from := range map[string]*registry.Session{
		"running":  blockedSession(registry.StateRunning, time.Hour, now),
		"stale":    blockedSession(registry.StateStale, time.Hour, now),
		"starting": blockedSession(registry.StateStarting, 5*time.Second, now), // inside the URL-scrape window
	} {
		applyPaneState(from, panes, quiet, screens, now, 30*time.Minute, 2*time.Minute)

		if from.State != registry.StateBlocked {
			t.Errorf("%s: state = %q, want blocked", name, from.State)
		}

		if !strings.Contains(from.Note, "Do you want to create second-probe.txt?") {
			t.Errorf("%s: the note must carry the question, got %q", name, from.Note)
		}
	}
}

// A quiet log is exactly what a blocked session looks like, so without this a
// session waiting on a prompt for half an hour was reported as merely "stale".
func TestAQuietBlockedSessionIsBlockedNotStale(t *testing.T) {
	now := time.Now().UTC()
	s := blockedSession(registry.StateRunning, 2*time.Hour, now)

	applyPaneState(s, map[string]tmux.Pane{"s": {Session: "s", PID: 1}},
		map[string]logStat{"/log": {ModTime: now.Add(-2 * time.Hour)}},
		map[string]string{"s": realPrompt(t)}, now, 30*time.Minute, 2*time.Minute)

	if s.State != registry.StateBlocked {
		t.Fatalf("state = %q", s.State)
	}
}

func TestSessionLeavesBlockedWhenTheScreenChangesAndTheNoteGoes(t *testing.T) {
	now := time.Now().UTC()
	panes := map[string]tmux.Pane{"s": {Session: "s", PID: 1}}
	fresh := map[string]logStat{"/log": {ModTime: now}}

	s := blockedSession(registry.StateRunning, time.Hour, now)
	applyPaneState(s, panes, fresh, map[string]string{"s": realPrompt(t)}, now, 30*time.Minute, 2*time.Minute)

	if s.State != registry.StateBlocked {
		t.Fatalf("setup: %q", s.State)
	}

	// The human answered; the screen is now an idle prompt.
	applyPaneState(s, panes, fresh, map[string]string{"s": "● done\n❯ \n⏸ manual mode on\n"}, now, 30*time.Minute, 2*time.Minute)

	if s.State != registry.StateRunning || s.Note != "" {
		t.Errorf("must return to running with the note cleared: %q / %q", s.State, s.Note)
	}
}

func TestADeadPaneIsNeverBlocked(t *testing.T) {
	now := time.Now().UTC()
	s := blockedSession(registry.StateRunning, time.Hour, now)

	applyPaneState(s, map[string]tmux.Pane{"s": {Session: "s", Dead: true, ExitStatus: 0}},
		nil, map[string]string{"s": realPrompt(t)}, now, 30*time.Minute, 2*time.Minute)

	if s.State != registry.StateStopped {
		t.Errorf("a finished session that left a prompt on screen is stopped, got %q", s.State)
	}
}

func TestParseScreensRoundTrip(t *testing.T) {
	out := "@@SCR agent-a-1\nline one\n\x1b[31mred\x1b[0m\n@@SCR agent-b-2\nonly line\n"

	got := parseScreens(out)

	if got["agent-b-2"] != "only line\n" && got["agent-b-2"] != "only line" {
		t.Errorf("%q", got["agent-b-2"])
	}

	if strings.Contains(got["agent-a-1"], "\x1b") || !strings.Contains(got["agent-a-1"], "red") {
		t.Errorf("escapes must be stripped: %q", got["agent-a-1"])
	}
}

// End to end through Reconcile and the registry, including the Active filter:
// if blocked were left out of it, a blocked session would stop being reconciled
// and could never return to running.
func TestReconcileMarksBlockedAndThenUnblocks(t *testing.T) {
	f, host := newTestFleet(t)
	ctx := context.Background()

	host.paneOutput = "https://claude.ai/code/session_x\n"

	s, err := f.Launch(ctx, LaunchRequest{Host: "homelab", Path: "notes-svc", TaskID: "needs-you", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}

	if waitForURL(t, f, s.ID) == "" {
		t.Fatal("no url")
	}

	host.listPanes = "agent-notes-svc-needs-you|1|0||claude"
	host.logStat = "/srv/agent-logs/notes-svc/needs-you.log|" + nowEpoch() + "|10"
	host.screens = map[string]string{"agent-notes-svc-needs-you": realPrompt(t)}

	if err := f.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	got, _ := f.Get(ctx, s.ID)
	if got.State != registry.StateBlocked || !strings.Contains(got.Note, "waiting for you") {
		t.Fatalf("after the first sweep: %q / %q", got.State, got.Note)
	}

	// Listed as active, so it keeps being reconciled.
	active, _ := f.Registry().List(ctx, registry.Filter{Active: true})
	if len(active) != 1 {
		t.Fatalf("a blocked session must stay in the active set, got %d", len(active))
	}

	// The human approves; the screen no longer shows the prompt.
	host.screens = map[string]string{"agent-notes-svc-needs-you": "● done\n❯ \n"}

	if err := f.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	got, _ = f.Get(ctx, s.ID)
	if got.State != registry.StateRunning || got.Note != "" {
		t.Errorf("after approval: %q / %q", got.State, got.Note)
	}
}

func TestBlockedIsAnActiveState(t *testing.T) {
	if !registry.StateBlocked.Active() {
		t.Error("blocked is a live session and must count as active (it can be sent to and killed)")
	}
}
