package fleet

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/tmux"
)

func TestParseLogStats(t *testing.T) {
	stats := parseLogStats("/srv/agent-logs/a.log|1758300000|4096\n/srv/agent-logs/b.log|1758300100|0\njunk\n")

	if len(stats) != 2 {
		t.Fatalf("got %d stats, want 2", len(stats))
	}

	if got := stats["/srv/agent-logs/a.log"]; got.Size != 4096 || got.ModTime.Unix() != 1758300000 {
		t.Errorf("stat parsed wrong: %+v", got)
	}
}

func TestSplitSweep(t *testing.T) {
	panes, logs := splitSweep("sess|1|0||sh\n" + sweepSep + "\n/a.log|1|2\n")

	if panes != "sess|1|0||sh\n" {
		t.Errorf("panes = %q", panes)
	}
	if logs != "\n/a.log|1|2\n" {
		t.Errorf("logs = %q", logs)
	}

	// No marker at all (a host where the sweep half-failed) must not panic.
	p, l := splitSweep("only panes")
	if p != "only panes" || l != "" {
		t.Errorf("degraded split = %q / %q", p, l)
	}
}

// The state machine, driven directly — this is what decides whether an
// operator sees "running" or "stopped (127)".
func TestApplyPaneState(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	staleAfter := 30 * time.Minute
	scrapeWindow := 2 * time.Minute

	fresh := map[string]logStat{"/log": {ModTime: now.Add(-time.Minute)}}
	old := map[string]logStat{"/log": {ModTime: now.Add(-2 * time.Hour)}}

	newSession := func(state registry.State, age time.Duration, url string) *registry.Session {
		return &registry.Session{
			TmuxSession: "s", LogPath: "/log", State: state,
			CreatedAt: now.Add(-age), RemoteURL: url,
		}
	}

	t.Run("alive becomes running", func(t *testing.T) {
		s := newSession(registry.StateStarting, time.Hour, "https://claude.ai/code/x")
		panes := map[string]tmux.Pane{"s": {Session: "s", PID: 42}}

		applyPaneState(s, panes, fresh, nil, now, staleAfter, scrapeWindow)

		if s.State != registry.StateRunning {
			t.Errorf("state = %q, want running", s.State)
		}
		if s.PanePID != 42 {
			t.Errorf("pane pid = %d", s.PanePID)
		}
	})

	t.Run("holds starting while the URL scrape still has time", func(t *testing.T) {
		s := newSession(registry.StateStarting, 10*time.Second, "")
		panes := map[string]tmux.Pane{"s": {Session: "s", PID: 42}}

		applyPaneState(s, panes, fresh, nil, now, staleAfter, scrapeWindow)

		if s.State != registry.StateStarting {
			t.Errorf("state = %q, want starting — the scrape window has not closed", s.State)
		}
	})

	t.Run("gives up on starting once the window closes", func(t *testing.T) {
		s := newSession(registry.StateStarting, time.Hour, "")
		panes := map[string]tmux.Pane{"s": {Session: "s", PID: 42}}

		applyPaneState(s, panes, fresh, nil, now, staleAfter, scrapeWindow)

		if s.State != registry.StateRunning {
			t.Errorf("state = %q, want running", s.State)
		}
	})

	t.Run("quiet log becomes stale", func(t *testing.T) {
		s := newSession(registry.StateRunning, time.Hour, "u")
		panes := map[string]tmux.Pane{"s": {Session: "s", PID: 42}}

		applyPaneState(s, panes, old, nil, now, staleAfter, scrapeWindow)

		if s.State != registry.StateStale {
			t.Errorf("state = %q, want stale", s.State)
		}
	})

	t.Run("dead pane records the exit status", func(t *testing.T) {
		s := newSession(registry.StateRunning, time.Hour, "u")
		panes := map[string]tmux.Pane{"s": {Session: "s", Dead: true, ExitStatus: 127}}

		applyPaneState(s, panes, fresh, nil, now, staleAfter, scrapeWindow)

		if s.State != registry.StateStopped {
			t.Errorf("state = %q, want stopped", s.State)
		}
		if s.ExitStatus == nil || *s.ExitStatus != 127 {
			t.Errorf("exit status = %v, want 127", s.ExitStatus)
		}
		if s.EndedAt == nil {
			t.Error("ended_at was not set")
		}
	})

	t.Run("a clean exit is status 0, not unset", func(t *testing.T) {
		s := newSession(registry.StateRunning, time.Hour, "u")
		panes := map[string]tmux.Pane{"s": {Session: "s", Dead: true, ExitStatus: 0}}

		applyPaneState(s, panes, fresh, nil, now, staleAfter, scrapeWindow)

		if s.ExitStatus == nil || *s.ExitStatus != 0 {
			t.Errorf("exit status = %v, want 0", s.ExitStatus)
		}
	})

	t.Run("missing session becomes stopped", func(t *testing.T) {
		s := newSession(registry.StateRunning, time.Hour, "u")

		applyPaneState(s, map[string]tmux.Pane{}, fresh, nil, now, staleAfter, scrapeWindow)

		if s.State != registry.StateStopped {
			t.Errorf("state = %q, want stopped", s.State)
		}
		if s.Note == "" {
			t.Error("a vanished session should say so in its note")
		}
	})

	// Liveness must not depend on the control URL: a session whose Remote
	// Control connection dropped is still running (need.md).
	t.Run("no URL does not affect liveness", func(t *testing.T) {
		s := newSession(registry.StateRunning, time.Hour, "")
		panes := map[string]tmux.Pane{"s": {Session: "s", PID: 7}}

		applyPaneState(s, panes, fresh, nil, now, staleAfter, scrapeWindow)

		if s.State != registry.StateRunning {
			t.Errorf("state = %q, want running", s.State)
		}
	})
}

// The sweep must cost one round trip per host, not one per session.
func TestReconcileIsOneRoundTripPerHost(t *testing.T) {
	f, host := newTestFleet(t)
	ctx := context.Background()

	host.paneOutput = "https://claude.ai/code/session_sweep\n"

	var ids []string

	for _, task := range []string{"a", "b", "c"} {
		s, err := f.Launch(ctx, LaunchRequest{
			Host: "homelab", Path: "notes-svc", TaskID: task, Prompt: "go",
		})
		if err != nil {
			t.Fatal(err)
		}

		ids = append(ids, s.ID)
	}

	// Let the scrape land, so nothing is still being held in "starting"
	// waiting for a URL — that rule has its own test above.
	for _, id := range ids {
		if waitForURL(t, f, id) == "" {
			t.Fatalf("session %s never got a URL", id)
		}
	}

	host.listPanes = "agent-notes-svc-a|1|0||claude\nagent-notes-svc-b|2|1|0|sh"
	host.logStat = "/srv/agent-logs/notes-svc/a.log|" + nowEpoch() + "|10"

	before := len(host.Commands())

	if err := f.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	sweeps := 0

	for _, cmd := range host.Commands()[before:] {
		if strings.Contains(cmd, "list-panes -a") {
			sweeps++
		}
	}

	if sweeps != 1 {
		t.Errorf("reconcile issued %d sweeps for 3 sessions on 1 host, want 1", sweeps)
	}

	sessions, err := f.Registry().List(ctx, registry.Filter{})
	if err != nil {
		t.Fatal(err)
	}

	states := map[string]registry.State{}
	for _, s := range sessions {
		states[s.TaskID] = s.State
	}

	if states["a"] != registry.StateRunning {
		t.Errorf("a = %q, want running", states["a"])
	}
	if states["b"] != registry.StateStopped {
		t.Errorf("b = %q, want stopped", states["b"])
	}
	// c was never listed by tmux at all.
	if states["c"] != registry.StateStopped {
		t.Errorf("c = %q, want stopped", states["c"])
	}
}

func nowEpoch() string { return strconv.FormatInt(time.Now().Unix(), 10) }
