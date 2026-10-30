package tmux

import (
	"strings"
	"testing"
)

func TestParsePanes(t *testing.T) {
	// Real shape of `tmux list-panes -a -F ...`: a live pane reports an empty
	// pane_dead_status, which is exactly why the fields are separated
	// explicitly instead of split on whitespace.
	out := strings.Join([]string{
		"agent-notes-svc-fix-tax|12345|0||node",
		"agent-api-kit-bump|0|1|127|",
		"agent-cli-kit-lint|4242|1|0|sh",
	}, "\n")

	panes := ParsePanes(out)

	if len(panes) != 3 {
		t.Fatalf("got %d panes, want 3", len(panes))
	}

	live := panes["agent-notes-svc-fix-tax"]
	if live.Dead || live.PID != 12345 || live.Command != "node" {
		t.Errorf("live pane parsed wrong: %+v", live)
	}
	// Alive means no exit status, which must not be mistaken for 0.
	if live.ExitStatus != -1 {
		t.Errorf("live pane ExitStatus = %d, want -1", live.ExitStatus)
	}

	crashed := panes["agent-api-kit-bump"]
	if !crashed.Dead || crashed.ExitStatus != 127 {
		t.Errorf("crashed pane parsed wrong: %+v", crashed)
	}

	clean := panes["agent-cli-kit-lint"]
	if !clean.Dead || clean.ExitStatus != 0 {
		t.Errorf("cleanly-exited pane parsed wrong: %+v", clean)
	}
}

func TestParsePanesPrefersLivePane(t *testing.T) {
	// A human attached and opened a second pane in the same session. The
	// agent's live pane is the one that matters.
	out := "s|1|1|0|sh\ns|2|0||claude\n"

	if p := ParsePanes(out)["s"]; p.Dead || p.PID != 2 {
		t.Errorf("got %+v, want the live pane (pid 2)", p)
	}
}

func TestParsePanesIgnoresJunk(t *testing.T) {
	out := "no server running on /tmp/tmux-1000/default\n\n   \nsess|1|0||sh\n"

	panes := ParsePanes(out)

	if len(panes) != 1 {
		t.Fatalf("got %d panes, want 1: %+v", len(panes), panes)
	}
}

// -J joins wrapped lines. Without it a long Remote Control URL is captured in
// pieces and the scrape silently misses (SPEC.md §16.1).
func TestCapturePaneCmdJoinsWrappedLines(t *testing.T) {
	cmd := CapturePaneCmd("agent-x-y", 400)

	if !strings.Contains(cmd, " -J ") {
		t.Fatalf("capture-pane must pass -J, got: %s", cmd)
	}
	if !strings.Contains(cmd, "-S -400") {
		t.Errorf("capture-pane should request 400 lines of scrollback, got: %s", cmd)
	}
}

// A session name is pinned with "=" so tmux matches it exactly rather than as
// a prefix — otherwise "agent-x" would also match "agent-x-2".
func TestSessionCommandsPinTheName(t *testing.T) {
	if cmd := AttachCommand("agent-x"); !strings.Contains(cmd, "'=agent-x'") {
		t.Errorf("session target is not pinned with '=': %s", cmd)
	}
}

// Pane-target commands need "=session:", not "=session". Real tmux rejects
// pipe-pane and send-keys outright on a bare session target, and — worse —
// capture-pane returns empty output instead of failing, which would make the
// URL scrape silently never match.
func TestPaneCommandsUseAPaneTarget(t *testing.T) {
	var piped, sent string

	if err := PipePane(t.Context(), recorder(&piped), "agent-x", "/logs/x.log"); err != nil {
		t.Fatal(err)
	}
	if err := SendKeys(t.Context(), recorder(&sent), "agent-x", "hello"); err != nil {
		t.Fatal(err)
	}

	for name, cmd := range map[string]string{
		"capture-pane": CapturePaneCmd("agent-x", 10),
		"pipe-pane":    piped,
		"send-keys":    sent,
	} {
		if !strings.Contains(cmd, "'=agent-x:'") {
			t.Errorf("%s must use a pane target '=agent-x:', got: %s", name, cmd)
		}

		// The bare session form, with nothing after it, is the bug.
		if strings.Contains(cmd, "'=agent-x'") {
			t.Errorf("%s uses a bare session target: %s", name, cmd)
		}
	}
}

func TestListPanesCmdIsSingleRoundTrip(t *testing.T) {
	cmd := ListPanesCmd()

	// -a: every session on the host in one command, not one call per session.
	if !strings.Contains(cmd, "list-panes -a") {
		t.Errorf("reconcile sweep must list all panes at once, got: %s", cmd)
	}
}

// remain-on-exit is a *window* option: `set-option -t =session` fails with
// "no such window" on real tmux. It must use -w and a window target, and it
// must be chained onto new-session so a fast-failing agent cannot exit first.
func TestNewSessionSetsRemainOnExitAsAWindowOption(t *testing.T) {
	var got string

	err := NewSession(t.Context(), recorder(&got), "agent-x-y", "/w", "/w/launch.sh")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got, "set-option -w -t '=agent-x-y:' remain-on-exit on") {
		t.Errorf("remain-on-exit must be a window option on a window target, got: %s", got)
	}
	if strings.Contains(got, "set-option -t '=agent-x-y'") {
		t.Errorf("session-scoped set-option does not work for remain-on-exit: %s", got)
	}
	// One tmux invocation, not two.
	if strings.Count(got, "tmux ") != 1 {
		t.Errorf("new-session and set-option should be chained into one command: %s", got)
	}
	if !strings.Contains(got, "';'") {
		t.Errorf("the chain separator must reach tmux as a literal ';': %s", got)
	}
}
