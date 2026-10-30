// Package tmux builds the tmux command lines agent-manager runs on remote
// hosts and parses what comes back.
//
// It talks to a transport.Transport, not to sshx — so the same code drives a
// pod over kubectl exec once that transport exists.
package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/joomcode/errorx"

	"github.com/aldrinleal/agent-manager-public/transport"
	"github.com/aldrinleal/agent-manager-public/util"
)

var Errors = errorx.NewNamespace("tmux")

var ErrTmux = Errors.NewType("tmux")

// fieldSep separates fields in our -F format strings. Session names are
// slugged to [A-Za-z0-9_-] (util.Slug), so this can never appear in one.
const fieldSep = "|"

// paneFormat is what the reconcile sweep reads.
//
// pane_dead_status is empty while a pane is alive, which is why the fields are
// separated explicitly instead of split on whitespace.
var paneFormat = strings.Join([]string{
	"#{session_name}",
	"#{pane_pid}",
	"#{pane_dead}",
	"#{pane_dead_status}",
	"#{pane_current_command}",
}, fieldSep)

// Pane is one tmux pane as reported by list-panes.
type Pane struct {
	Session    string
	PID        int
	Dead       bool
	ExitStatus int // valid only when Dead; -1 when tmux reported none
	Command    string
}

// Version returns the remote tmux version string, or an error if tmux is
// missing. This doubles as the tmux half of preflight.
func Version(ctx context.Context, t transport.Transport) (string, error) {
	res, err := t.Run(ctx, "tmux -V")
	if err != nil {
		return "", err
	}

	if !res.OK() {
		return "", ErrTmux.New("tmux not available (exit %d): %s", res.ExitCode, res.Err())
	}

	return res.Out(), nil
}

// HasSession reports whether a session exists.
func HasSession(ctx context.Context, t transport.Transport, session string) (bool, error) {
	res, err := t.Run(ctx, fmt.Sprintf("tmux has-session -t %s 2>/dev/null", util.ShellQuote(target(session))))
	if err != nil {
		return false, err
	}

	return res.OK(), nil
}

// NewSession starts a detached session running script in dir, with
// remain-on-exit set on its window.
//
// script is a path to a file we wrote — never an inline command. Passing a
// command string here would put it through tmux's parser and then a shell,
// two layers of quoting over a user-supplied prompt (SPEC.md §7.1).
//
// remain-on-exit keeps a finished pane visible and dead rather than letting
// the session disappear, so the reconciler can read a real exit status
// instead of inferring one from the session vanishing (SPEC.md §7.4). Two
// details, both learned the hard way:
//
//   - it is a *window* option, so it needs `-w` and a window target
//     ("session:", the session's current window). `set-option -t =session`
//     fails with "no such window".
//   - it is chained onto new-session with a literal ";" rather than sent as a
//     second command, so there is no window in which a fast-failing agent can
//     exit before the option is set.
func NewSession(ctx context.Context, t transport.Transport, session, dir, script string) error {
	cmd := fmt.Sprintf("tmux new-session -d -s %s -c %s %s ';' set-option -w -t %s remain-on-exit on",
		util.ShellQuote(session), util.ShellQuote(dir), util.ShellQuote(script),
		util.ShellQuote(paneTarget(session)))

	res, err := t.Run(ctx, cmd)
	if err != nil {
		return err
	}

	if !res.OK() {
		return ErrTmux.New("creating session %q failed (exit %d): %s", session, res.ExitCode, res.Err())
	}

	return nil
}

// PipePane tees the pane's output to logPath.
//
// This is deliberately tmux's own mechanism rather than script(1): it keeps
// the remote requirement at exactly linux + tmux + git (SPEC.md §7.4).
func PipePane(ctx context.Context, t transport.Transport, session, logPath string) error {
	inner := "cat >> " + util.ShellQuote(logPath)

	cmd := fmt.Sprintf("mkdir -p %s && tmux pipe-pane -o -t %s %s",
		util.ShellQuote(dirOf(logPath)),
		util.ShellQuote(paneTarget(session)),
		util.ShellQuote(inner))

	res, err := t.Run(ctx, cmd)
	if err != nil {
		return err
	}

	if !res.OK() {
		return ErrTmux.New("pipe-pane on %q failed (exit %d): %s", session, res.ExitCode, res.Err())
	}

	return nil
}

// CapturePane returns the last n lines of a session's pane, scrollback
// included.
//
// -J is not optional: it joins wrapped lines. Without it a Remote Control URL
// wrapped at the terminal width is captured in pieces and the scrape in
// runner/claudecode.go silently misses it (SPEC.md §16.1).
func CapturePane(ctx context.Context, t transport.Transport, session string, n int) (string, error) {
	res, err := t.Run(ctx, CapturePaneCmd(session, n))
	if err != nil {
		return "", err
	}

	// A gone session is not an error here — callers want "" and move on.
	if !res.OK() {
		return "", nil
	}

	return util.StripANSI(res.Stdout), nil
}

// CapturePaneCmd is CapturePane's command line, exposed so the URL scrape can
// batch it with a log tail into one round trip.
func CapturePaneCmd(session string, n int) string {
	return fmt.Sprintf("tmux capture-pane -p -J -S -%d -t %s 2>/dev/null",
		n, util.ShellQuote(paneTarget(session)))
}

// ListPanesCmd lists every pane on the host in one shot. The reconcile sweep
// uses this instead of probing sessions one at a time (SPEC.md §9).
func ListPanesCmd() string {
	return fmt.Sprintf("tmux list-panes -a -F %s 2>/dev/null", util.ShellQuote(paneFormat))
}

// ListPanes runs ListPanesCmd and parses it.
func ListPanes(ctx context.Context, t transport.Transport) (map[string]Pane, error) {
	res, err := t.Run(ctx, ListPanesCmd())
	if err != nil {
		return nil, err
	}

	// Exit 1 with no output is "no server running" — no sessions, not a failure.
	if !res.OK() && strings.TrimSpace(res.Stdout) == "" {
		return map[string]Pane{}, nil
	}

	return ParsePanes(res.Stdout), nil
}

// ParsePanes turns list-panes output into panes keyed by session name.
//
// A session with several panes keeps the first live one; that is the agent's
// pane, and any extras are things a human opened while attached.
func ParsePanes(out string) map[string]Pane {
	panes := map[string]Pane{}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(util.StripANSI(line))
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, fieldSep, 5)
		if len(parts) < 5 {
			continue
		}

		pid, _ := strconv.Atoi(strings.TrimSpace(parts[1]))

		exitStatus := -1
		if s := strings.TrimSpace(parts[3]); s != "" {
			if v, err := strconv.Atoi(s); err == nil {
				exitStatus = v
			}
		}

		pane := Pane{
			Session:    strings.TrimSpace(parts[0]),
			PID:        pid,
			Dead:       strings.TrimSpace(parts[2]) == "1",
			ExitStatus: exitStatus,
			Command:    strings.TrimSpace(parts[4]),
		}

		if prev, seen := panes[pane.Session]; seen && !prev.Dead {
			continue
		}

		panes[pane.Session] = pane
	}

	return panes
}

// KillSession stops a session. A session that is already gone is success.
func KillSession(ctx context.Context, t transport.Transport, session string) error {
	cmd := fmt.Sprintf("tmux kill-session -t %s 2>/dev/null || true", util.ShellQuote(target(session)))

	if _, err := t.Run(ctx, cmd); err != nil {
		return err
	}

	return nil
}

// SendKeys types text into a session's pane and presses Enter — how you give
// a running agent a new instruction without Remote Control.
//
// -l sends the text literally, so tmux does not interpret "Enter", "C-c" or
// anything else inside it as a key name.
func SendKeys(ctx context.Context, t transport.Transport, session, text string) error {
	cmd := fmt.Sprintf("tmux send-keys -t %s -l %s && tmux send-keys -t %s Enter",
		util.ShellQuote(paneTarget(session)), util.ShellQuote(text), util.ShellQuote(paneTarget(session)))

	res, err := t.Run(ctx, cmd)
	if err != nil {
		return err
	}

	if !res.OK() {
		return ErrTmux.New("send-keys to %q failed (exit %d): %s", session, res.ExitCode, res.Err())
	}

	return nil
}

// AttachCommand is the command a human runs to take over a session.
func AttachCommand(session string) string {
	return fmt.Sprintf("tmux attach -t %s", util.ShellQuote(target(session)))
}

// target pins a session name so tmux reads it as a session and not as a
// prefix match. Used by the commands that take a *session* target:
// has-session, kill-session, attach.
func target(session string) string { return "=" + session }

// paneTarget names the active pane of a session's current window.
//
// Required by every command that takes a pane or window target — pipe-pane,
// capture-pane, send-keys, and window options like remain-on-exit. Handing
// those a bare session target fails, and capture-pane fails *silently*:
// it prints nothing rather than erroring, so with stderr discarded the URL
// scrape would simply never match. The trailing ":" is what makes tmux
// resolve the session to its current window.
func paneTarget(session string) string { return "=" + session + ":" }

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "."
}
