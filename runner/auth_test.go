package runner

import "testing"

func TestParseAuthStatusLoggedIn(t *testing.T) {
	// Newlines collapsed to spaces, exactly as preflight hands it over.
	a := ParseAuthStatus(`{ "loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty", "email": "dev@example.com", "subscriptionType": "max" }`)

	if a.LoggedIn == nil || !*a.LoggedIn || a.Email != "dev@example.com" || a.Subscription != "max" || a.Method != "claude.ai" {
		t.Errorf("%+v", a)
	}
}

func TestParseAuthStatusLoggedOutIsADefiniteFalse(t *testing.T) {
	a := ParseAuthStatus(`{ "loggedIn": false, "authMethod": "none", "configDirectory": "/x" }`)

	if a.LoggedIn == nil || *a.LoggedIn {
		t.Fatalf("logged out must be a definite false, got %+v", a)
	}
}

// Unknown must stay unknown: failing a preflight on garbage would lock out
// every host running a claude that predates `auth`.
func TestParseAuthStatusUnknownStaysNil(t *testing.T) {
	for name, in := range map[string]string{
		"empty":         "",
		"whitespace":    "  \n ",
		"old claude":    "error: unknown command 'auth'",
		"not json":      "Logged in as someone",
		"json no field": `{"hello": 1}`,
	} {
		if a := ParseAuthStatus(in); a.LoggedIn != nil {
			t.Errorf("%s: want unknown, got %v", name, *a.LoggedIn)
		}
	}
}

func TestClaudeAuthURL(t *testing.T) {
	u := "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&state=abc"

	if got := ClaudeAuthURL.FindString("Opening browser to sign in…\nIf the browser didn't open, visit: " + u + "\nPaste code here if prompted >"); got != u {
		t.Errorf("got %q", got)
	}

	// It must NOT match a Remote Control link, or the two scrapes could cross.
	if ClaudeAuthURL.MatchString("https://claude.ai/code/session_01abc") {
		t.Error("the auth pattern matched a Remote Control URL")
	}
}
