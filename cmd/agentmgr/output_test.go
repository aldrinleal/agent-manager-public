package main

import "testing"

func TestShortVersion(t *testing.T) {
	cases := map[string]string{
		"git version 2.43.0":  "2.43.0",
		"tmux 3.5a":           "3.5a",
		"2.1.0 (Claude Code)": "2.1.0",
		"tmux next-3.6":       "3.6",
		"":                    "",
		"weird":               "weird",
	}

	for in, want := range cases {
		if got := shortVersion(in); got != want {
			t.Errorf("shortVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
