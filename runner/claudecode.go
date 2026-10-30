package runner

import (
	"regexp"
	"strings"

	"github.com/aldrinleal/agent-manager-public/util"
)

func init() { register(ClaudeCode{}) }

// ClaudeCode launches `claude --remote-control` in a worktree.
//
// The Remote Control URL it prints is the only way to reach the session from
// a browser, and there is no API that lists those — capturing it here at
// launch is the whole reason this tool exists (need.md, "solução prática:
// registry próprio").
type ClaudeCode struct{}

func (ClaudeCode) Name() string { return "claude-code" }

func (ClaudeCode) Probes() []Probe {
	return []Probe{
		{
			Name:     "claude",
			Command:  "command -v claude >/dev/null 2>&1 && claude --version",
			Required: true,
		},
		{
			// `|| true` is load-bearing: a logged-out claude prints its JSON
			// and exits 1, and the preflight only emits a probe's marker on
			// success. Without it the marker is lost and the JSON leaks into
			// the NEXT probe's output.
			Name:     "auth",
			Command:  "claude auth status 2>/dev/null || true",
			Required: false,
		},
	}
}

// claudeURL matches a Remote Control session link.
//
// Matched against ANSI-stripped, line-joined output only — see
// tmux.CapturePane's -J and util.StripANSI (SPEC.md §16.1, §16.2).
var claudeURL = regexp.MustCompile(`https://claude\.ai/code/[A-Za-z0-9_/?=&.\-]+`)

func (ClaudeCode) URLPattern() *regexp.Regexp { return claudeURL }

// LaunchScript renders the script tmux runs.
//
// Note what is *not* here: the prompt. It is read from PromptFile at runtime
// with "$(cat ...)", so a prompt containing quotes, newlines, backticks or $(
// cannot break out — the only things interpolated are paths and labels that
// agent-manager generated and sanitized itself.
func (ClaudeCode) LaunchScript(spec LaunchSpec) (string, error) {
	if spec.Worktree == "" {
		return "", ErrScript.New("claude-code: no worktree")
	}
	if spec.PromptFile == "" {
		return "", ErrScript.New("claude-code: no prompt file")
	}

	var b strings.Builder

	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Rendered by agent-manager. Do not edit — it is rewritten on every launch.\n")
	b.WriteString("# session: " + spec.SessionLabel + "\n\n")

	b.WriteString(envLines(spec.Env, util.ShellQuote))
	b.WriteString("\n")

	b.WriteString("cd " + util.ShellQuote(spec.Worktree) + " || exit 90\n\n")

	// Fail loudly inside the pane rather than exiting instantly with a bare
	// 127 — remain-on-exit keeps this visible for whoever attaches.
	b.WriteString("if ! command -v claude >/dev/null 2>&1; then\n")
	b.WriteString("  echo 'agent-manager: `claude` is not on PATH for this shell.' >&2\n")
	b.WriteString("  echo \"agent-manager: PATH=$PATH\" >&2\n")
	b.WriteString("  echo 'agent-manager: set a PATH under this host'\"'\"'s env: in hosts.yaml' >&2\n")
	b.WriteString("  exit 127\n")
	b.WriteString("fi\n\n")

	args := []string{"--remote-control", spec.SessionLabel}
	args = append(args, spec.ExtraArgs...)

	b.WriteString("exec claude " + util.JoinArgs(args...) +
		" \"$(cat " + util.ShellQuote(spec.PromptFile) + ")\"\n")

	return b.String(), nil
}
