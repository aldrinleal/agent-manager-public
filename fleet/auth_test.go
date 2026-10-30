package fleet

import (
	"context"
	"strings"
	"testing"
	"time"
)

const oauthURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&state=xyz"

const loginScreen = "Opening browser to sign in…\nIf the browser didn't open, visit: " + oauthURL + "\nPaste code here if prompted >"

func TestAuthLoginStartReturnsTheURLAndRunsClaudeInTmux(t *testing.T) {
	f, host := newTestFleet(t)
	host.paneOutput = loginScreen

	out, err := f.AuthLoginStart(context.Background(), "homelab")
	if err != nil {
		t.Fatal(err)
	}

	if out.URL != oauthURL || out.Reused || out.Session != "agentmgr-auth-homelab" {
		t.Errorf("%+v", out)
	}

	script, ok := host.file("/srv/agent-logs/auth/homelab.login.sh")
	if !ok {
		t.Fatal("login script was not written")
	}

	// exec, so killing the tmux session hangs claude up directly (an earlier
	// attempt under `timeout` left an orphan), and the host's PATH so a
	// per-user claude install is found.
	if !strings.Contains(script, "exec claude auth login") || !strings.Contains(script, "export PATH='/home/dev/.local/bin:/usr/bin'") {
		t.Errorf("script:\n%s", script)
	}

	if !host.ran("tmux new-session", "agentmgr-auth-homelab", "/srv/agent-logs/auth/homelab.login.sh") {
		t.Error("no tmux session was started")
	}

	// Nothing about this session — the URL, later the code — goes to a log.
	if host.ran("pipe-pane") {
		t.Error("the auth session must not be piped to a log file")
	}
}

func TestAuthLoginStartReusesAWaitingSession(t *testing.T) {
	f, host := newTestFleet(t)
	host.hasSession = true
	host.paneOutput = loginScreen

	out, err := f.AuthLoginStart(context.Background(), "homelab")
	if err != nil {
		t.Fatal(err)
	}

	if !out.Reused || out.URL != oauthURL {
		t.Errorf("%+v", out)
	}

	if host.ran("tmux new-session") {
		t.Error("a second `claude auth login` must not race the first for the same code")
	}
}

func TestAuthLoginStartWithNoURLKillsTheSessionAndShowsTheScreen(t *testing.T) {
	f, host := newTestFleet(t)
	host.paneOutput = "claude: command not found"

	// The 25s scrape deadline is real time; run it with an already-cancelled
	// budget instead of waiting it out.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := f.AuthLoginStart(ctx, "homelab")
	if err == nil || !strings.Contains(err.Error(), "command not found") {
		t.Fatalf("the screen should be in the error, got %v", err)
	}

	if !host.ran("kill-session", "agentmgr-auth-homelab") {
		t.Error("a login session that produced no URL must not be left running")
	}
}

func TestAuthLoginStartRejectsAJumpHost(t *testing.T) {
	f, _ := newTestFleet(t)

	if _, err := f.AuthLoginStart(context.Background(), "bastion"); err == nil {
		t.Fatal("a jump-only host has no claude to log in")
	}
}

// What may be typed into a terminal on a remote host is bounded: a code, and
// nothing that could be a second command or a control sequence.
func TestAuthLoginCodeRefusesAnythingThatIsNotACode(t *testing.T) {
	f, host := newTestFleet(t)
	host.hasSession = true

	for name, code := range map[string]string{
		"empty":     "",
		"short":     "abc",
		"newline":   "validcode123\nrm -rf /",
		"space":     "valid code 123",
		"semicolon": "abc123;reboot",
		"escape":    "abc123\x1b[2J",
		"backtick":  "abc`id`123",
		"too long":  strings.Repeat("a", 601),
	} {
		if _, err := f.AuthLoginCode(context.Background(), "homelab", code); err == nil {
			t.Errorf("%s: accepted %q", name, code)
		}
	}

	if len(host.Commands()) != 0 {
		t.Errorf("a refused code must not reach the host: %v", host.Commands())
	}
}

func TestAuthLoginCodeWithNoSessionIsNotFound(t *testing.T) {
	f, _ := newTestFleet(t)

	_, err := f.AuthLoginCode(context.Background(), "homelab", "validcode-123#abc")
	if err == nil || !strings.Contains(err.Error(), "no login is waiting") {
		t.Fatalf("got %v", err)
	}
}

func TestAuthLoginCodeSuccessKillsTheSessionAndRefreshesPreflight(t *testing.T) {
	f, host := newTestFleet(t)
	f.authPollEvery, f.authPollFor = time.Millisecond, 200*time.Millisecond
	host.hasSession = true
	host.listPanes = "agentmgr-auth-homelab|1|1|0|sh" // dead, exit 0

	res, err := f.AuthLoginCode(context.Background(), "homelab", "validcode-123#abc")
	if err != nil {
		t.Fatal(err)
	}

	if !res.LoggedIn || res.Email != "dev@example.com" || res.Check == nil || !res.Check.OK {
		t.Fatalf("%+v", res)
	}

	if !host.ran("send-keys", "validcode-123#abc") || !host.ran("kill-session", "agentmgr-auth-homelab") {
		t.Error("code should be typed in and the session cleaned up")
	}

	if c, _ := f.Registry().HostCheck(context.Background(), "homelab"); c == nil || c.AuthLoggedIn == nil || !*c.AuthLoggedIn {
		t.Errorf("preflight was not re-run and recorded: %+v", c)
	}
}

func TestAuthLoginCodeWhenTheLoginProcessFailed(t *testing.T) {
	f, host := newTestFleet(t)
	f.authPollEvery, f.authPollFor = time.Millisecond, 200*time.Millisecond
	host.hasSession = true
	host.listPanes = "agentmgr-auth-homelab|1|1|1|sh" // dead, exit 1
	host.paneOutput = "Error: invalid code"

	res, err := f.AuthLoginCode(context.Background(), "homelab", "wrongcode-123")
	if err != nil {
		t.Fatal(err)
	}

	if res.LoggedIn || res.Retry || !strings.Contains(res.Screen, "invalid code") {
		t.Errorf("%+v", res)
	}

	// A screen that includes the login URL must have it redacted.
	host.paneOutput = loginScreen + " Login failed: That code wasn't accepted."
	host.hasSession = true

	res2, _ := f.AuthLoginCode(context.Background(), "homelab", "wrongcode-456")
	if strings.Contains(res2.Screen, "oauth/authorize") || !strings.Contains(res2.Screen, "<login url>") {
		t.Errorf("the OAuth URL should be redacted from the screen:\n%s", res2.Screen)
	}
}

// The subtle one. On a host that is ALREADY logged in, `claude auth status`
// reads true before and after, so judging by it alone would call a rejected
// code a success. Success must mean the login process itself finished.
func TestAuthLoginCodeOnAnAlreadyLoggedInHostIsNotFooledByTheStatus(t *testing.T) {
	f, host := newTestFleet(t)
	f.authPollEvery, f.authPollFor = time.Millisecond, 50*time.Millisecond
	host.hasSession = true
	host.listPanes = "agentmgr-auth-homelab|1|0||claude" // ALIVE: it re-prompted
	host.paneOutput = "Invalid code. Paste code here >"
	// host.authOut already says loggedIn:true from newFakeHost.

	res, err := f.AuthLoginCode(context.Background(), "homelab", "wrongcode-123")
	if err != nil {
		t.Fatal(err)
	}

	if res.LoggedIn {
		t.Fatal("a rejected code on an already-logged-in host was reported as success")
	}

	if !res.Retry || !strings.Contains(res.Screen, "Invalid code") {
		t.Errorf("should offer a retry with the screen: %+v", res)
	}

	if host.ran("kill-session") {
		t.Error("the session must stay up so another code can be tried")
	}
}

func TestAuthLoginSessionExpiresUnused(t *testing.T) {
	f, host := newTestFleet(t)
	f.cfg.AuthLoginTimeout = 40 * time.Millisecond
	host.paneOutput = loginScreen

	if _, err := f.AuthLoginStart(context.Background(), "homelab"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if host.ran("kill-session", "agentmgr-auth-homelab") {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("an abandoned login session was never killed")
}

func TestAuthLoginCancelKillsTheSessionAndIsIdempotent(t *testing.T) {
	f, host := newTestFleet(t)
	ctx := context.Background()

	if was, err := f.AuthLoginCancel(ctx, "homelab"); err != nil || was {
		t.Fatalf("nothing waiting: want (false, nil), got (%v, %v)", was, err)
	}

	host.hasSession = true

	was, err := f.AuthLoginCancel(ctx, "homelab")
	if err != nil || !was {
		t.Fatalf("a waiting login should be cancelled: (%v, %v)", was, err)
	}

	if !host.ran("kill-session", "agentmgr-auth-homelab") {
		t.Error("the session was not killed")
	}
}

// The real pane of a failed login: the reason sits ABOVE a screenful of blank
// padding and a "Pane is dead" line. Taking the last N raw lines lost it.
func TestLastLinesSkipsBlankPaddingSoTheReasonSurvives(t *testing.T) {
	screen := "Opening browser to sign in…\nPaste code here if prompted > Login failed: That code wasn't accepted.\n" +
		strings.Repeat("\n", 11) + "Pane is dead (status 1, Wed Oct 28 21:58:21 2026)"

	got := lastLines(screen, 3)

	if !strings.Contains(got, "That code wasn't accepted") {
		t.Errorf("the failure reason was cut off:\n%s", got)
	}

	if strings.Contains(got, "\n\n") {
		t.Errorf("blank lines should be dropped:\n%q", got)
	}
}

// remain-on-exit keeps a finished login's pane, URL included. Reusing it would
// hand out a link to a process that is gone.
func TestAuthLoginStartReplacesADeadSession(t *testing.T) {
	f, host := newTestFleet(t)
	host.hasSession = true
	host.listPanes = "agentmgr-auth-homelab|1|1|1|sh" // dead
	host.paneOutput = loginScreen

	out, err := f.AuthLoginStart(context.Background(), "homelab")
	if err != nil {
		t.Fatal(err)
	}

	if out.Reused {
		t.Error("a dead session's URL was handed out as a waiting login")
	}

	if !host.ran("kill-session", "agentmgr-auth-homelab") || !host.ran("tmux new-session", "agentmgr-auth-homelab") {
		t.Error("the dead session should be killed and a fresh one started")
	}
}
