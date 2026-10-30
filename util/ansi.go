package util

import "regexp"

// ansiPattern matches CSI/OSC escape sequences plus the handful of two-byte
// escapes Claude Code's TUI emits. Terminal output has to go through this
// before any regex match, or escape sequences land mid-URL and the scrape in
// runner/claudecode.go silently misses (SPEC.md §16.2).
// The CSI class follows ECMA-48 exactly: parameter bytes 0x30-0x3F (which
// includes the private-use prefixes < = > ?, not just ?), then intermediate
// bytes 0x20-0x2F, then a final byte 0x40-0x7E. Claude Code's TUI emits
// plenty of the private forms — ESC[<u, ESC[>4;2m, ESC[>0q — and a class of
// only [0-9;?] leaves their opening bracket and prefix behind as visible
// garbage in every captured line.
var ansiPattern = regexp.MustCompile(
	`\x1b\[[0-?]*[ -/]*[@-~]` + // CSI ... final byte
		`|\x1b\][^\a\x1b]*(?:\a|\x1b\\)` + // OSC ... BEL or ST
		`|\x1b[P^_][^\x1b]*(?:\x1b\\|\a)` + // DCS/PM/APC ... ST
		`|\x1b[()*+][A-Za-z0-9]` + // charset selection
		`|\x1b[=>ENOc78MD]` + // single-char escapes
		`|[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`, // stray control chars (keeps \t \n \r)
)

// StripANSI removes terminal escape sequences and stray control characters
// from s, leaving tabs, newlines and carriage returns alone.
func StripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}
