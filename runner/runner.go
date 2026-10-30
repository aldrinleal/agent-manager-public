// Package runner knows how to start one kind of coding agent.
//
// A runner is deliberately thin: a set of preflight probes, a launch script,
// and a pattern for finding a control URL in the agent's output. Adding
// codex/grok/opencode means adding one file here, not touching fleet.
package runner

import (
	"regexp"
	"sort"
	"strings"

	"github.com/joomcode/errorx"
)

var Errors = errorx.NewNamespace("runner")

var (
	ErrUnknownRunner = Errors.NewType("unknown_runner")
	ErrScript        = Errors.NewType("script")
)

// Probe is one preflight check beyond the universal linux/tmux/git set.
type Probe struct {
	// Name is what the failure message calls it, e.g. "claude".
	Name string
	// Command runs on the host; non-zero exit means the probe failed.
	Command string
	// Required fails the whole preflight when false-y; optional probes only
	// annotate the result.
	Required bool
}

// LaunchSpec is everything a runner needs to render its launch script.
type LaunchSpec struct {
	// SessionLabel is what the agent names itself, e.g. "notes-svc:fix-tax".
	SessionLabel string
	// Worktree is the directory the agent runs in.
	Worktree string
	// PromptFile holds the task prompt. It is read by the script, never
	// interpolated into it — the prompt must not pass through a shell.
	PromptFile string
	// Env is exported at the top of the script. This is where PATH lives, so
	// a per-user `claude` install is findable from a non-interactive shell.
	Env map[string]string
	// ExtraArgs are appended to the agent's command line.
	ExtraArgs []string
}

// Runner starts one kind of agent.
type Runner interface {
	// Name is the identifier used in hosts.yaml and the registry.
	Name() string

	// Probes are the runner-specific preflight checks.
	Probes() []Probe

	// LaunchScript renders the /bin/sh script written to the host and handed
	// to `tmux new-session`. Returning a script rather than a command string
	// is what keeps the prompt out of tmux's parser (SPEC.md §7.1).
	LaunchScript(spec LaunchSpec) (string, error)

	// URLPattern matches the agent's control URL in its terminal output.
	// nil means this runner publishes no URL.
	URLPattern() *regexp.Regexp
}

var registered = map[string]Runner{}

func register(r Runner) { registered[r.Name()] = r }

// Get returns a runner by name.
func Get(name string) (Runner, error) {
	if name == "" {
		name = "claude-code"
	}

	if r, ok := registered[name]; ok {
		return r, nil
	}

	return nil, ErrUnknownRunner.New("no runner named %q (known: %s)", name, strings.Join(Names(), ", "))
}

// Names lists every registered runner.
func Names() []string {
	out := make([]string, 0, len(registered))
	for name := range registered {
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}

// envLines renders the script's exported environment, sorted so the script is
// stable and diffable across launches.
func envLines(env map[string]string, quote func(string) string) string {
	if len(env) == 0 {
		return ""
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString("export " + k + "=" + quote(env[k]) + "\n")
	}

	return b.String()
}
