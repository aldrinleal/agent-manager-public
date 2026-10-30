package util

import "strings"

// Slug collapses every character outside [A-Za-z0-9_-] into a single dash and
// trims leading/trailing dashes.
//
// tmux treats "." and ":" specially in target specifications (window.pane,
// session:window), so session names built from user input have to be run
// through this — see SPEC.md §7.2.
func Slug(s string) string {
	var b strings.Builder

	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}

	return strings.Trim(b.String(), "-")
}
