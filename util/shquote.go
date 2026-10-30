package util

import "strings"

// ShellQuote wraps s in single quotes, escaping any single quotes inside it,
// so it survives /bin/sh word splitting intact.
//
// This is only ever applied to values agent-manager generates itself (paths,
// session names, remote/branch names) — never to a user-supplied prompt. See
// SPEC.md §7.1: prompts are written to a file over the SSH session's raw
// stdin and never pass through a shell at all.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellQuoteAll quotes every element of args.
func ShellQuoteAll(args ...string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = ShellQuote(a)
	}
	return out
}

// JoinArgs quotes every argument and joins them with spaces, producing one
// runnable /bin/sh command line.
func JoinArgs(args ...string) string {
	return strings.Join(ShellQuoteAll(args...), " ")
}
