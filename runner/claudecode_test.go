package runner

import (
	"strings"
	"testing"
)

func TestClaudeCodeLaunchScript(t *testing.T) {
	script, err := ClaudeCode{}.LaunchScript(LaunchSpec{
		SessionLabel: "notes-svc:fix-tax",
		Worktree:     "/srv/agent-workspaces/notes-svc/fix-tax",
		PromptFile:   "/srv/agent-workspaces/notes-svc/fix-tax/.agent-prompt.md",
		Env:          map[string]string{"PATH": "/home/dev/.local/bin:/usr/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"#!/bin/sh",
		"export PATH='/home/dev/.local/bin:/usr/bin'",
		"cd '/srv/agent-workspaces/notes-svc/fix-tax'",
		"exec claude '--remote-control' 'notes-svc:fix-tax'",
		`"$(cat '/srv/agent-workspaces/notes-svc/fix-tax/.agent-prompt.md')"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q:\n%s", want, script)
		}
	}
}

// The whole point of writing the prompt to a file: a prompt containing quotes,
// newlines, backticks or $( ) must not be able to reach the script at all.
// LaunchSpec has no prompt field, so this is structural — but assert it, since
// "just interpolate it" is the obvious wrong refactor for someone later.
func TestLaunchScriptNeverCarriesThePrompt(t *testing.T) {
	const nasty = "'; rm -rf / #\n$(whoami)\n`id`"

	script, err := ClaudeCode{}.LaunchScript(LaunchSpec{
		SessionLabel: "repo:task",
		Worktree:     "/w",
		PromptFile:   "/w/.agent-prompt.md",
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(script, "rm -rf") || strings.Contains(script, "whoami") {
		t.Fatalf("prompt content leaked into the launch script:\n%s", script)
	}

	// The prompt is read at runtime, from the file, inside double quotes — so
	// even a prompt like the above is one argument.
	if !strings.Contains(script, `"$(cat '/w/.agent-prompt.md')"`) {
		t.Errorf("script should read the prompt from its file:\n%s", script)
	}

	_ = nasty
}

// A worktree path with a single quote in it must still produce a valid script.
func TestLaunchScriptQuotesAwkwardPaths(t *testing.T) {
	script, err := ClaudeCode{}.LaunchScript(LaunchSpec{
		SessionLabel: "repo:task",
		Worktree:     "/srv/it's/here",
		PromptFile:   "/srv/it's/here/.agent-prompt.md",
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(script, `cd '/srv/it'\''s/here'`) {
		t.Errorf("path with a quote was not escaped:\n%s", script)
	}
}

func TestLaunchScriptRequiresWorktreeAndPrompt(t *testing.T) {
	if _, err := (ClaudeCode{}).LaunchScript(LaunchSpec{PromptFile: "/p"}); err == nil {
		t.Error("expected an error with no worktree")
	}
	if _, err := (ClaudeCode{}).LaunchScript(LaunchSpec{Worktree: "/w"}); err == nil {
		t.Error("expected an error with no prompt file")
	}
}

func TestURLPattern(t *testing.T) {
	re := ClaudeCode{}.URLPattern()

	cases := map[string]string{
		"Open https://claude.ai/code/session_01ABCdef in your browser": "https://claude.ai/code/session_01ABCdef",
		"  https://claude.ai/code/s_abc?foo=bar&baz=1 ":                "https://claude.ai/code/s_abc?foo=bar&baz=1",
		"https://claude.ai/code/session_x.y-z/more":                    "https://claude.ai/code/session_x.y-z/more",
		"nothing here":                       "",
		"https://example.com/code/session_x": "",
	}

	for in, want := range cases {
		if got := re.FindString(in); got != want {
			t.Errorf("FindString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGetDefaultsToClaudeCode(t *testing.T) {
	r, err := Get("")
	if err != nil {
		t.Fatal(err)
	}

	if r.Name() != "claude-code" {
		t.Errorf("default runner = %q, want claude-code", r.Name())
	}

	if _, err := Get("nope"); err == nil {
		t.Error("expected an error for an unknown runner")
	}
}

func TestEnvLinesAreSorted(t *testing.T) {
	out := envLines(map[string]string{"Z": "1", "A": "2", "M": "3"}, func(s string) string { return "'" + s + "'" })

	want := "export A='2'\nexport M='3'\nexport Z='1'\n"
	if out != want {
		t.Errorf("envLines = %q, want %q", out, want)
	}
}
