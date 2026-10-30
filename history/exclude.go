package history

import (
	"regexp"
	"strings"
)

// Excluder decides which projects are not worth reporting at all.
type Excluder struct {
	// Substrings are matched case-insensitively anywhere in the project path.
	// The default (scratch, /.openclaw, /.claude) is the 2026-09-22 selection:
	// scratch repos were excluded by the owner, and the other two are tool
	// directories rather than projects.
	Substrings []string
}

// DefaultExcludes is what AGENTMGR_HISTORY_EXCLUDE falls back to.
var DefaultExcludes = []string{"scratch", "/.openclaw", "/.claude"}

// homeDir matches a bare home directory, which is where claude is run from when
// it is not in a project at all.
var homeDir = regexp.MustCompile(`^/(?:home|Users)/[^/]+/?$|^/root/?$`)

// Excluded reports whether project is filtered out, and why.
func (e Excluder) Excluded(project string) (bool, string) {
	if homeDir.MatchString(project) {
		return true, "home directory"
	}

	lower := strings.ToLower(project)

	for _, s := range e.Substrings {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" && strings.Contains(lower, s) {
			return true, "matches exclude " + s
		}
	}

	return false, ""
}

// ParseExcludes splits a comma-separated AGENTMGR_HISTORY_EXCLUDE.
func ParseExcludes(s string) []string {
	var out []string

	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}

	return out
}
