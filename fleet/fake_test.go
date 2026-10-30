package fleet

import (
	"context"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"sync"

	"github.com/aldrinleal/agent-manager-public/transport"
)

// fakeHost stands in for a real machine: it records every command, answers
// the ones the launch path issues, and remembers files written to it.
//
// This lets the whole of Launch/Reconcile be exercised for real — the same
// command strings that would go over SSH — without needing a host.
type fakeHost struct {
	mu sync.Mutex

	commands []string
	files    map[string]string
	modes    map[string]fs.FileMode

	// knobs
	hasSession     bool
	worktreeSeen   bool
	paneOutput     string
	listPanes      string
	logStat        string
	failOn         string // any command containing this exits 1
	missingTools   map[string]bool
	head           string
	dirty          string
	conflicts      string
	sweepOut       string // answer to a status sweep
	statusOut      string // answer to a single-path status
	logOut         string
	mergeBase      string
	refMissing     bool
	pushFail       bool
	screens        map[string]string // visible screen per tmux session, for the sweep
	historyOut     string
	historyMissing bool
	resolveOut     string
	authOut        string // what `claude auth status` prints; "" => logged in
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		authOut:      `{ "loggedIn": true, "authMethod": "claude.ai", "email": "dev@example.com", "subscriptionType": "max" }`,
		files:        map[string]string{},
		modes:        map[string]fs.FileMode{},
		missingTools: map[string]bool{},
		head:         "abc1234def5678\n",
	}
}

func (f *fakeHost) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.commands...)
}

// ran reports whether any recorded command contains every given fragment.
func (f *fakeHost) ran(fragments ...string) bool {
	for _, cmd := range f.Commands() {
		all := true

		for _, frag := range fragments {
			if !strings.Contains(cmd, frag) {
				all = false

				break
			}
		}

		if all {
			return true
		}
	}

	return false
}

func (f *fakeHost) file(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	v, ok := f.files[path]

	return v, ok
}

// mvTarget pulls the destination out of the WriteFile command shape
// (`... && mv -f 'tmp' 'dest'`).
var mvTarget = regexp.MustCompile(`mv -f '(?:[^']|'\\'')*' '((?:[^']|'\\'')*)'\s*$`)

func unquote(s string) string { return strings.ReplaceAll(s, `'\''`, "'") }

func (f *fakeHost) Run(ctx context.Context, cmd string) (transport.Result, error) {
	return f.RunStdin(ctx, cmd, nil)
}

func (f *fakeHost) RunStdin(_ context.Context, cmd string, stdin io.Reader) (transport.Result, error) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()

	if f.failOn != "" && strings.Contains(cmd, f.failOn) {
		return transport.Result{ExitCode: 1, Stderr: "fake: failing on " + f.failOn}, nil
	}

	switch {
	case strings.Contains(cmd, "history.jsonl"):
		if f.historyMissing {
			return transport.Result{ExitCode: 1, Stderr: "cat: /home/x/.claude/history.jsonl: No such file or directory"}, nil
		}

		return transport.Result{Stdout: f.historyOut}, nil

	case strings.Contains(cmd, "@@P|"):
		return transport.Result{Stdout: f.resolveOut}, nil

	case strings.Contains(cmd, "@@PATH "):
		return transport.Result{Stdout: f.sweepOut}, nil

	case strings.Contains(cmd, "--porcelain=v2"):
		return transport.Result{Stdout: f.v2Status()}, nil

	case strings.Contains(cmd, "'rev-parse' '--verify'") && f.refMissing:
		return transport.Result{ExitCode: 1}, nil

	case strings.Contains(cmd, "'push'"):
		if f.pushFail {
			return transport.Result{ExitCode: 1, Stderr: " ! [rejected]        agent/preview -> agent/preview (non-fast-forward)"}, nil
		}

		return transport.Result{Stderr: "remote: Create a pull request for 'agent/preview' on GitHub by visiting:\nremote:      https://github.com/o/r/pull/new/agent/preview\n"}, nil

	case strings.Contains(cmd, "'log' '--oneline'"):
		return transport.Result{Stdout: f.logOut}, nil

	case strings.Contains(cmd, "'merge-base'"):
		return transport.Result{Stdout: f.mergeBase}, nil

	// Preflight: a chain of `if { probe; } then echo MARKERname; fi`.
	case strings.Contains(cmd, "@@AGENTMGR@@"):
		return transport.Result{Stdout: f.preflight(cmd)}, nil

	case cmd == "uname -s":
		if f.missingTools["os"] {
			return transport.Result{Stdout: "Darwin\n"}, nil
		}

		return transport.Result{Stdout: "Linux\n"}, nil

	case strings.Contains(cmd, "claude auth status"):
		return transport.Result{Stdout: f.authOut}, nil

	// WriteFile.
	case strings.Contains(cmd, "cat > ") && strings.Contains(cmd, "mv -f "):
		body := ""
		if stdin != nil {
			raw, _ := io.ReadAll(stdin)
			body = string(raw)
		}

		if m := mvTarget.FindStringSubmatch(strings.TrimSpace(cmd)); m != nil {
			f.mu.Lock()
			f.files[unquote(m[1])] = body
			f.mu.Unlock()
		}

		return transport.Result{}, nil

	// gitx quotes every argument, so the fragments match that shape.
	case strings.Contains(cmd, "'rev-parse' '--is-inside-work-tree'"):
		return transport.Result{Stdout: "true\n"}, nil

	case strings.Contains(cmd, "'rev-parse' 'HEAD'"):
		return transport.Result{Stdout: f.head}, nil

	case strings.Contains(cmd, "'status' '--porcelain'"):
		return transport.Result{Stdout: f.dirty}, nil

	case strings.Contains(cmd, "'diff' '--stat'"):
		return transport.Result{Stdout: " app/tax.go | 12 ++++++------\n 1 file changed\n"}, nil

	case strings.Contains(cmd, "'diff' '--name-only' '--diff-filter=U'"):
		return transport.Result{Stdout: f.conflicts}, nil

	case strings.Contains(cmd, "test -e "):
		if f.worktreeSeen {
			return transport.Result{}, nil
		}

		return transport.Result{ExitCode: 1}, nil

	case strings.Contains(cmd, "tmux has-session"):
		if f.hasSession {
			return transport.Result{}, nil
		}

		return transport.Result{ExitCode: 1}, nil

	case strings.Contains(cmd, "list-panes -a"):
		// The reconcile sweep batches list-panes, a stat over the logs and the
		// visible screen of every session into one command.
		if strings.Contains(cmd, "stat -c") {
			var b strings.Builder

			b.WriteString(f.listPanes + "\n" + sweepSep + "\n" + f.logStat + "\n" + screensSep + "\n")

			for name, scr := range f.screens {
				b.WriteString(screenMark + name + "\n" + scr + "\n")
			}

			return transport.Result{Stdout: b.String()}, nil
		}

		return transport.Result{Stdout: f.listPanes}, nil

	case strings.Contains(cmd, "capture-pane"):
		out := f.paneOutput

		// The scrape batches capture-pane with a log tail in one command.
		return transport.Result{Stdout: out}, nil

	}

	return transport.Result{}, nil
}

// preflight synthesizes the marker-delimited output runProbes expects.
func (f *fakeHost) preflight(cmd string) string {
	var b strings.Builder

	answers := map[string]string{
		"os":     "Linux",
		"tmux":   "tmux 3.4",
		"git":    "git version 2.43.0",
		"claude": "2.1.0 (Claude Code)",
		"auth":   f.authOut,
	}

	for _, line := range strings.Split(cmd, "\n") {
		idx := strings.Index(line, "@@AGENTMGR@@")
		if idx < 0 {
			continue
		}

		name := strings.Trim(line[idx+len("@@AGENTMGR@@"):], "'; fi")

		if f.missingTools[name] {
			continue
		}

		if answer, ok := answers[name]; ok {
			b.WriteString(answer + "\n")
		}

		b.WriteString("@@AGENTMGR@@" + name + "\n")
	}

	return b.String()
}

func (f *fakeHost) WriteFile(ctx context.Context, path string, mode fs.FileMode, r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	f.mu.Lock()
	f.files[path] = string(raw)
	f.modes[path] = mode
	f.commands = append(f.commands, "WriteFile "+path)
	f.mu.Unlock()

	return nil
}

func (f *fakeHost) Close() error { return nil }

// v2Status answers a `git status --porcelain=v2` compound command: statusOut
// verbatim when a test set it, otherwise a valid status derived from f.dirty
// (porcelain v1 " M path" lines), so one knob drives both status paths.
func (f *fakeHost) v2Status() string {
	if f.statusOut != "" {
		return f.statusOut
	}

	var b strings.Builder

	b.WriteString("# branch.oid abc1234\n# branch.head master\n# branch.upstream origin/master\n# branch.ab +0 -0\n")

	for _, l := range strings.Split(strings.TrimSpace(f.dirty), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			path := strings.TrimSpace(l[strings.IndexByte(l, ' ')+1:])
			b.WriteString("1 .M N... 100644 100644 100644 h h " + path + "\n")
		}
	}

	b.WriteString("@@INPROGRESS\n")

	return b.String()
}
