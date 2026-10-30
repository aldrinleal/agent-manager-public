package util

import "testing"

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":       `'plain'`,
		"with space":  `'with space'`,
		`it's`:        `'it'\''s'`,
		"$(rm -rf /)": `'$(rm -rf /)'`,
		"`backtick`":  "'`backtick`'",
		`a'b'c`:       `'a'\''b'\''c'`,
		"":            `''`,
	}

	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"fix-checkout-tax": "fix-checkout-tax",
		"fix checkout tax": "fix-checkout-tax",
		"Fix/Checkout:Tax": "Fix-Checkout-Tax",
		"...leading":       "leading",
		"trailing...":      "trailing",
		"a...b":            "a-b",
		"under_score":      "under_score",
		"acentuação":       "acentua-o",
		"!!!":              "",
	}

	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// tmux session names must not contain "." or ":" — tmux reads those as
// window/pane separators in a target specification.
func TestSlugStripsTmuxTargetSeparators(t *testing.T) {
	for _, in := range []string{"a.b", "a:b", "a.b:c"} {
		got := Slug(in)

		for _, bad := range []rune{'.', ':'} {
			for _, r := range got {
				if r == bad {
					t.Fatalf("Slug(%q) = %q, still contains %q", in, got, string(bad))
				}
			}
		}
	}
}

func TestStripANSI(t *testing.T) {
	// A colored, cursor-moved URL the way a TUI actually emits it.
	in := "\x1b[1m\x1b[38;5;208mOpen\x1b[0m \x1b[4mhttps://claude.ai/code/session_abc123\x1b[0m\x1b[K\r\n"

	got := StripANSI(in)

	want := "Open https://claude.ai/code/session_abc123\r\n"
	if got != want {
		t.Errorf("StripANSI = %q, want %q", got, want)
	}
}

func TestStripANSIKeepsWhitespace(t *testing.T) {
	in := "a\tb\nc\r\n"

	if got := StripANSI(in); got != in {
		t.Errorf("StripANSI ate whitespace: %q -> %q", in, got)
	}
}

func TestStripANSIOSC(t *testing.T) {
	// OSC title sequence, BEL-terminated — Claude Code sets the window title.
	in := "\x1b]0;claude\x07hello"

	if got := StripANSI(in); got != "hello" {
		t.Errorf("StripANSI = %q, want %q", got, "hello")
	}
}

func TestJoinArgs(t *testing.T) {
	got := JoinArgs("git", "-C", "/a b", "merge", "--ff-only", "origin/main")

	want := `'git' '-C' '/a b' 'merge' '--ff-only' 'origin/main'`
	if got != want {
		t.Errorf("JoinArgs = %q, want %q", got, want)
	}
}

// Claude Code's TUI emits CSI sequences with private parameter prefixes
// (< = >), not just "?". A class of only [0-9;?] leaves "ESC[<u" behind as
// visible "[<u" garbage in every captured line.
func TestStripANSIPrivateParameterSequences(t *testing.T) {
	cases := map[string]string{
		"\x1b[<u\x1b[>5u\x1b[>4;2mhello": "hello",
		"\x1b[>0qworld":                  "world",
		"\x1b[?25lhidden\x1b[?25h":       "hidden",
		"\x1b[=3hmode":                   "mode",
		"a\x1b[2Kb":                      "ab",
	}

	for in, want := range cases {
		if got := StripANSI(in); got != want {
			t.Errorf("StripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}

// Whatever comes out must contain no escape character at all.
func TestStripANSILeavesNoEscapes(t *testing.T) {
	in := "\x1b[<u\x1b]0;t\x07\x1b(B\x1b=\x1b[38;5;208m x \x1b[0m\x1bM\x1b[>4;2m"

	got := StripANSI(in)

	for _, r := range got {
		if r == 0x1b {
			t.Fatalf("escape survived stripping: %q", got)
		}
	}

	if got != " x " {
		t.Errorf("StripANSI = %q, want %q", got, " x ")
	}
}
