package runner

import (
	"encoding/json"
	"regexp"
	"strings"
)

// AuthStatus is what `claude auth status` reports.
type AuthStatus struct {
	// LoggedIn is nil when the output could not be interpreted at all — an
	// older claude with no `auth` subcommand, or garbage. Unknown is NOT the
	// same as logged out, and only a definite false should fail a preflight.
	LoggedIn     *bool
	Email        string
	Subscription string
	Method       string
}

// ParseAuthStatus reads `claude auth status` output.
//
// Observed shapes (2026-10-28, claude 2.1.x):
//
//	logged in   {"loggedIn": true, "authMethod": "claude.ai", "email": "...", "subscriptionType": "max", ...}  exit 0
//	logged out  {"loggedIn": false, "authMethod": "none", ...}                                               exit 1
//
// The exit code differs, so callers must capture stdout regardless of it.
// Preflight joins a probe's lines with spaces before this sees them, which is
// harmless to JSON.
func ParseAuthStatus(out string) AuthStatus {
	out = strings.TrimSpace(out)
	if out == "" {
		return AuthStatus{}
	}

	var raw struct {
		LoggedIn         *bool  `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		Email            string `json:"email"`
		SubscriptionType string `json:"subscriptionType"`
	}

	if err := json.Unmarshal([]byte(out), &raw); err != nil || raw.LoggedIn == nil {
		return AuthStatus{}
	}

	return AuthStatus{
		LoggedIn:     raw.LoggedIn,
		Email:        raw.Email,
		Subscription: raw.SubscriptionType,
		Method:       raw.AuthMethod,
	}
}

// ClaudeAuthURL matches the OAuth link `claude auth login` prints when it
// cannot open a browser. Distinct from the Remote Control URL pattern: that
// one is claude.ai/code/..., this is .../oauth/authorize?...
//
// Matched against ANSI-stripped, wrap-joined (capture-pane -J) output only:
// the URL is a few hundred characters and certainly wraps in an 80-column pane.
var ClaudeAuthURL = regexp.MustCompile(`https://claude\.(?:com|ai)/\S*oauth/authorize\?\S+`)

// AuthLoginScript renders the script tmux runs for `claude auth login`.
//
// It is a script, not an inline tmux command, for the same reason the launch
// script is (SPEC §7.1) — and because PATH has to be right: claude is a
// per-user install a non-interactive shell cannot find. `exec` makes claude
// the pane's own process, so killing the tmux session hangs IT up directly;
// under `timeout` an earlier attempt left an orphaned `claude auth login`
// behind on a host.
func AuthLoginScript(env map[string]string, quote func(string) string) string {
	return "#!/bin/sh\n# Rendered by agent-manager for `claude auth login`.\n\n" +
		envLines(env, quote) + "\nexec claude auth login\n"
}
