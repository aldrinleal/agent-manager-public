package fleet

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/runner"
	"github.com/aldrinleal/agent-manager-public/tmux"
	"github.com/aldrinleal/agent-manager-public/transport"
	"github.com/aldrinleal/agent-manager-public/util"
)

// authSessionPrefix names the tmux session that hosts `claude auth login`.
// Distinct from agent-<repo>-<task>, so reconcile and the console never mistake
// it for an agent.
const authSessionPrefix = "agentmgr-auth-"

// authCode bounds what may be typed into the login session. An OAuth code is
// opaque but plain: letters, digits and a few separators. Refusing anything
// else keeps this endpoint from being a way to type arbitrary text — newlines,
// escape sequences, shell fragments — into a terminal on a remote host.
var authCode = regexp.MustCompile(`^[A-Za-z0-9_\-#.~+/=%:]{6,600}$`)

func authSession(host string) string { return authSessionPrefix + util.Slug(host) }

// AuthLogin is a started (or reused) login session.
type AuthLogin struct {
	Host    string `json:"host"`
	Session string `json:"tmux_session"`
	// URL is the OAuth link to open in a browser — on any device. It carries
	// client parameters; it is returned only over the authenticated API and is
	// never logged above debug.
	URL string `json:"url"`
	// Reused: a login was already waiting on this host, so no second one was
	// started.
	Reused bool `json:"reused"`
	// ExpiresIn is how long the session will wait for the code, in seconds.
	ExpiresIn int      `json:"expires_in"`
	Warnings  []string `json:"warnings"`
}

// AuthLoginStart runs `claude auth login` in a tmux session on host, scrapes
// the OAuth URL it prints, and returns it. The human opens the URL, authorizes,
// and gets a code, which goes back through AuthLoginCode.
//
// There is no device flow and no --no-browser flag (SPEC/handover §4): the
// process waits on stdin for the code, which is why it lives in a tmux session
// that outlasts this request.
func (f *Fleet) AuthLoginStart(ctx context.Context, hostName string) (*AuthLogin, error) {
	host, err := f.inv.Host(hostName)
	if err != nil {
		return nil, err
	}

	if !host.IsLaunchTarget() {
		return nil, ErrBadTarget.New("host %q is role: %s; there is no claude to log in there", hostName, host.Role)
	}

	if host.Runner != "claude-code" {
		return nil, ErrBadTarget.New("host %q runs %q; auth login is only implemented for claude-code", hostName, host.Runner)
	}

	t, err := f.transportFor(hostName)
	if err != nil {
		return nil, err
	}

	execCtx, cancel := f.ctxWithExecTimeout(ctx)
	defer cancel()

	session := authSession(hostName)
	out := &AuthLogin{Host: hostName, Session: session, ExpiresIn: int(f.cfg.AuthLoginTimeout.Seconds()), Warnings: []string{}}

	// One login per host at a time. Reuse a waiting one rather than starting a
	// second `claude auth login` that would race the first for the same code.
	if exists, herr := tmux.HasSession(execCtx, t, session); herr != nil {
		return nil, herr
	} else if exists {
		// remain-on-exit keeps a FINISHED login's pane around, URL and all. A
		// dead pane's URL belongs to a process that no longer exists, so it
		// must never be handed out as "a login already waiting".
		if !f.paneDead(execCtx, t, session) {
			if url := f.scrapeAuthURL(execCtx, t, session); url != "" {
				out.URL, out.Reused = url, true
				f.armAuthExpiry(hostName, session)

				return out, nil
			}
		}

		// Dead, or alive with no URL in it (stuck): replace it.
		_ = tmux.KillSession(execCtx, t, session)
	}

	if check, _ := f.reg.HostCheck(execCtx, hostName); check != nil && check.AuthLoggedIn != nil && *check.AuthLoggedIn {
		out.Warnings = append(out.Warnings,
			fmt.Sprintf("%s is already logged in as %s; a successful login here REPLACES that login", hostName, check.AuthEmail))
	}

	script := runner.AuthLoginScript(host.Env, util.ShellQuote)
	scriptPath := fmt.Sprintf("%s/auth/%s.login.sh", strings.TrimRight(host.LogRoot, "/"), util.Slug(hostName))

	if err := t.WriteFile(execCtx, scriptPath, 0o700, strings.NewReader(script)); err != nil {
		return nil, err
	}

	// No pipe-pane: nothing about this session — the URL, and later the code —
	// is written to a log file on the host.
	if err := tmux.NewSession(execCtx, t, session, host.LogRoot, scriptPath); err != nil {
		return nil, err
	}

	// The URL is printed within a second or two; give it generous room.
	deadline := time.Now().Add(25 * time.Second)

	for {
		if url := f.scrapeAuthURL(execCtx, t, session); url != "" {
			out.URL = url
			f.armAuthExpiry(hostName, session)

			return out, nil
		}

		if time.Now().After(deadline) || execCtx.Err() != nil {
			break
		}

		time.Sleep(time.Second)
	}

	screen, _ := tmux.CapturePane(context.Background(), t, session, 30)
	_ = tmux.KillSession(context.Background(), t, session)

	return nil, ErrLaunch.New("claude auth login printed no login URL within 25s; its screen was:\n%s", lastLines(screen, 12))
}

// paneDead reports whether the session's process has exited. A session that
// tmux does not list is treated as alive: only an explicit "dead" replaces it.
func (f *Fleet) paneDead(ctx context.Context, t transport.Transport, session string) bool {
	panes, err := tmux.ListPanes(ctx, t)
	if err != nil {
		return false
	}

	p, ok := panes[session]

	return ok && p.Dead
}

// scrapeAuthURL reads the login URL out of a session's pane. capture-pane's -J
// (set in tmux.CapturePaneCmd) joins the wrapped lines; the URL is several
// hundred characters and certainly wraps.
func (f *Fleet) scrapeAuthURL(ctx context.Context, t transport.Transport, session string) string {
	screen, err := tmux.CapturePane(ctx, t, session, 400)
	if err != nil {
		return ""
	}

	return runner.ClaudeAuthURL.FindString(screen)
}

// armAuthExpiry (re)starts the countdown after which an unfinished login
// session is killed, so an abandoned one does not wait forever on a host.
func (f *Fleet) armAuthExpiry(hostName, session string) {
	if f.cfg.AuthLoginTimeout <= 0 {
		return
	}

	f.authMu.Lock()
	defer f.authMu.Unlock()

	if old := f.authTimers[hostName]; old != nil {
		old.Stop()
	}

	f.authTimers[hostName] = time.AfterFunc(f.cfg.AuthLoginTimeout, func() {
		t, err := f.transportFor(hostName)
		if err != nil {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), f.cfg.ExecTimeout)
		defer cancel()

		if err := tmux.KillSession(ctx, t, session); err == nil {
			log.Infof("fleet: login session for %s expired unused and was killed", hostName)
		}
	})
}

func (f *Fleet) disarmAuthExpiry(hostName string) {
	f.authMu.Lock()
	defer f.authMu.Unlock()

	if tm := f.authTimers[hostName]; tm != nil {
		tm.Stop()
		delete(f.authTimers, hostName)
	}
}

// AuthCodeResult is the outcome of submitting a login code.
type AuthCodeResult struct {
	Host     string              `json:"host"`
	LoggedIn bool                `json:"logged_in"`
	Email    string              `json:"email,omitempty"`
	Check    *registry.HostCheck `json:"check,omitempty"`
	// Screen is the tail of the login session's terminal when it did NOT
	// succeed — "invalid code", or still waiting — so the caller can show why.
	Screen string `json:"screen,omitempty"`
	// Retry: the login session is still waiting, so another code can be sent.
	Retry bool `json:"retry"`
}

// AuthLoginCode types the code into the waiting login session and reports
// whether the login completed.
//
// Success means the login PROCESS exited 0 and `claude auth status` is then
// logged in. Checking the status alone would be wrong on a host that is
// ALREADY logged in — it reads true before and after, so a wrong code would
// look like a success.
func (f *Fleet) AuthLoginCode(ctx context.Context, hostName, code string) (*AuthCodeResult, error) {
	if !authCode.MatchString(code) {
		return nil, ErrBadTarget.New("that does not look like a login code (letters, digits and - _ # . ~ + / = %% : only, 6 to 600 characters)")
	}

	if _, err := f.inv.Host(hostName); err != nil {
		return nil, err
	}

	t, err := f.transportFor(hostName)
	if err != nil {
		return nil, err
	}

	session := authSession(hostName)

	execCtx, cancel := context.WithTimeout(ctx, f.authPollFor+3*f.cfg.ExecTimeout)
	defer cancel()

	if exists, herr := tmux.HasSession(execCtx, t, session); herr != nil {
		return nil, herr
	} else if !exists {
		return nil, ErrNotFound.New("no login is waiting on %s (it may have expired); start one first", hostName)
	}

	if err := tmux.SendKeys(execCtx, t, session, code); err != nil {
		return nil, err
	}

	res := &AuthCodeResult{Host: hostName}
	deadline := time.Now().Add(f.authPollFor)

	for {
		panes, perr := tmux.ListPanes(execCtx, t)
		if perr == nil {
			if p, ok := panes[session]; ok && p.Dead {
				if p.ExitStatus == 0 {
					return f.finishLogin(execCtx, t, hostName, session, res)
				}

				// The process ended unhappily: nothing left to retry against.
				res.Screen = f.screenOf(t, session)
				_ = tmux.KillSession(context.Background(), t, session)
				f.disarmAuthExpiry(hostName)

				return res, nil
			}
		}

		if time.Now().After(deadline) || execCtx.Err() != nil {
			break
		}

		time.Sleep(f.authPollEvery)
	}

	// Still waiting: most likely the code was rejected and claude re-prompted.
	// Leave the session up so another code can be tried.
	res.Screen, res.Retry = f.screenOf(t, session), true

	return res, nil
}

func (f *Fleet) finishLogin(ctx context.Context, t transport.Transport, hostName, session string, res *AuthCodeResult) (*AuthCodeResult, error) {
	out, err := t.Run(ctx, "claude auth status 2>/dev/null || true")
	if err != nil {
		return nil, err
	}

	st := runner.ParseAuthStatus(out.Stdout)

	_ = tmux.KillSession(context.Background(), t, session)
	f.disarmAuthExpiry(hostName)

	if st.LoggedIn == nil || !*st.LoggedIn {
		res.Screen = "the login process exited 0 but `claude auth status` still says logged out"
		return res, nil
	}

	res.LoggedIn, res.Email = true, st.Email

	// Re-run preflight so the registry (and the next launch) sees the new state.
	if check, cerr := f.Check(ctx, hostName); check != nil {
		res.Check = check
		_ = cerr // a failed preflight for another reason is reported in check.Note
	}

	return res, nil
}

// screenOf returns the tail of a login session's terminal, for explaining a
// failure. The OAuth URL is redacted: whoever is reading "the code was not
// accepted" already has it, and it carries a PKCE challenge nobody else needs.
func (f *Fleet) screenOf(t transport.Transport, session string) string {
	s, _ := tmux.CapturePane(context.Background(), t, session, 30)

	return lastLines(runner.ClaudeAuthURL.ReplaceAllString(s, "<login url>"), 12)
}

// AuthStatus returns the last known login state of a host, running a
// preflight if there has never been one.
func (f *Fleet) AuthStatus(ctx context.Context, hostName string) (*registry.HostCheck, error) {
	if _, err := f.inv.Host(hostName); err != nil {
		return nil, err
	}

	if c, err := f.reg.HostCheck(ctx, hostName); err != nil || c != nil {
		return c, err
	}

	c, _ := f.Check(ctx, hostName)

	return c, nil
}

// lastLines returns the last n NON-BLANK lines. capture-pane returns the whole
// visible screen, so the bottom of it is padding: taking the last n raw lines
// of a dead login's pane returned eleven empty ones and "Pane is dead", and cut
// off the actual reason ("Login failed: That code wasn't accepted") above them.
func lastLines(s string, n int) string {
	var lines []string

	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimRight(l, " "))
		}
	}

	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return strings.Join(lines, "\n")
}

// AuthLoginCancel abandons a waiting login: kills its tmux session (which
// hangs up the `claude auth login` process, since it is the pane's own) and
// cancels the expiry timer. Not an error if none is waiting.
func (f *Fleet) AuthLoginCancel(ctx context.Context, hostName string) (bool, error) {
	if _, err := f.inv.Host(hostName); err != nil {
		return false, err
	}

	t, err := f.transportFor(hostName)
	if err != nil {
		return false, err
	}

	execCtx, cancel := f.ctxWithExecTimeout(ctx)
	defer cancel()

	session := authSession(hostName)

	exists, err := tmux.HasSession(execCtx, t, session)
	if err != nil {
		return false, err
	}

	f.disarmAuthExpiry(hostName)

	if !exists {
		return false, nil
	}

	return true, tmux.KillSession(execCtx, t, session)
}
