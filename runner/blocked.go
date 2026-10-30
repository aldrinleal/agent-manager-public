package runner

import (
	"regexp"
	"strings"
)

// Blocker is implemented by runners that can tell, from a terminal screen,
// that the agent has stopped and is waiting for a human.
type Blocker interface {
	// Blocked reports whether screen shows a prompt awaiting the human, and
	// the question it is asking ("" is fine when there is no readable one).
	Blocked(screen string) (bool, string)
}

var (
	// "❯ 1. Yes" — the highlighted option of Claude Code's selection menu. The
	// ❯ cursor is what separates a live menu from prose that happens to contain
	// a numbered list.
	cursorOption = regexp.MustCompile(`^\s*❯\s*\d\.\s+\S`)
	yesOption    = regexp.MustCompile(`^\s*(?:❯\s*)?1\.\s+Yes\b`)
	noOption     = regexp.MustCompile(`^\s*(?:❯\s*)?\d\.\s+No\b`)
	question     = regexp.MustCompile(`^\s*((?:Do you|Would you|Allow)\b.*\?)\s*$`)
)

// bottomLines is how much of the screen is considered. A permission prompt is
// drawn at the bottom; scanning further would let an old, answered prompt that
// is still scrolled into view count.
const bottomLines = 24

// Blocked recognises Claude Code's permission and trust prompts.
//
// Matched against a REAL screen captured from a session in manual mode asking to
// write a file (runner/testdata/permission-prompt.txt):
//
//	Do you want to create second-probe.txt?
//	❯ 1. Yes
//	  2. Yes, and switch to accept edits (...)
//	  3. No
//	Esc to cancel · Tab to amend
//
// The QUESTION varies by tool ("create X", "proceed", "make this edit to Y"), so
// it is not what is matched. The structure is: a menu with a ❯ cursor, a
// "1. Yes" option, a "No" option, and either the "Esc to cancel" footer or a
// question line. All must be present, which is what keeps an answer that merely
// quotes "1. Yes / 2. No" from reading as a prompt. The folder-trust prompt
// ("1. Yes, proceed / 2. No, exit") has the same shape and is covered too.
func (ClaudeCode) Blocked(screen string) (bool, string) {
	var lines []string

	for _, l := range strings.Split(screen, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimRight(l, " "))
		}
	}

	if len(lines) > bottomLines {
		lines = lines[len(lines)-bottomLines:]
	}

	var cursor, yes, no, footer bool

	why := ""

	for _, l := range lines {
		switch {
		case cursorOption.MatchString(l):
			cursor = true

			if yesOption.MatchString(l) {
				yes = true
			}
		case yesOption.MatchString(l):
			yes = true
		case noOption.MatchString(l):
			no = true
		}

		if strings.Contains(l, "Esc to cancel") {
			footer = true
		}

		if m := question.FindStringSubmatch(l); m != nil {
			footer = true
			why = strings.TrimSpace(m[1])
		}
	}

	if !(cursor && yes && no && footer) {
		return false, ""
	}

	if why == "" {
		why = "waiting for approval"
	}

	return true, why
}

var _ Blocker = ClaudeCode{}
