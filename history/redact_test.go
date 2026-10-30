package history

import (
	"strings"
	"testing"
)

// The fake credentials are assembled at run time so this file never contains a
// string a secret scanner would flag.
func fake(parts ...string) string { return strings.Join(parts, "") }

func TestRedactReplacesCredentialsButKeepsTheSentence(t *testing.T) {
	jwt := fake("eyJ", strings.Repeat("a", 12), ".", strings.Repeat("b", 12), ".", strings.Repeat("c", 12))

	cases := map[string]struct{ in, gone, want string }{
		"openrouter key":   {fake("replace the key (", "sk-or-v1-", strings.Repeat("5e2a", 8), ") in .env"), "sk-or-v1-", "[redacted:api-key]"},
		"openai key":       {fake("use ", "sk-", strings.Repeat("A1", 14)), "sk-AA", "[redacted:api-key]"},
		"anthropic key":    {fake("key ", "sk-ant-", strings.Repeat("x9", 14)), "sk-ant-x9", "[redacted:anthropic-key]"},
		"github token":     {fake("push with ", "ghp_", strings.Repeat("Q", 36)), "ghp_QQQ", "[redacted:github-token]"},
		"aws key id":       {fake("key ", "AKIA", "ABCDEFGHIJKLMNOP", " ok"), "AKIAABC", "[redacted:aws-key]"},
		"slack token":      {fake("hook ", "xoxb-", "123456789012-abcdefghij"), "xoxb-1234", "[redacted:slack-token]"},
		"jwt":              {"auth " + jwt + " expired", jwt, "[redacted:jwt]"},
		"token assignment": {fake("curl -H 'Authorization: Bearer ", strings.Repeat("Z", 40), "'"), strings.Repeat("Z", 40), "[redacted:secret]"},
		"password kv":      {fake("password=", strings.Repeat("p4ss", 8)), "p4ssp4ss", "[redacted:secret]"},
		"private key":      {"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAB3NzaC1yc2EAAAADAQABAAABAQ\n-----END OPENSSH PRIVATE KEY-----", "AAAAB3Nza", "[redacted:private-key]"},
	}

	for name, c := range cases {
		got := Redact(c.in)

		if strings.Contains(got, c.gone) {
			t.Errorf("%s: the secret survived: %q", name, got)
		}

		if !strings.Contains(got, c.want) {
			t.Errorf("%s: want marker %s in %q", name, c.want, got)
		}
	}
}

// Ordinary prose must pass through untouched: a redactor that eats real prompts
// would make the work log useless.
func TestRedactLeavesOrdinaryPromptsAlone(t *testing.T) {
	for _, s := range []string{
		"faz um pull/rebase e drena o DLQ",
		"the token expires in 5 minutes, refresh it",
		"set the password field to required",
		"git@github.com:aldrinleal/agent-manager-public.git",
		"see https://docs.openclaw.ai/clawhub/skill-format and ../api-kit",
		"sk- is the prefix", // a prefix alone is not a key
		"/home/dev/src/shop-agent/main.ts:120",
	} {
		if got := Redact(s); got != s {
			t.Errorf("changed an ordinary prompt:\n  in : %q\n  out: %q", s, got)
		}
	}
}

// What is stored is redacted; the dedup fingerprint still comes from the
// original text, so redaction cannot make two different prompts collapse.
func TestParseStoresRedactedTextButFingerprintsTheOriginal(t *testing.T) {
	key1 := fake("sk-", strings.Repeat("a", 30))
	key2 := fake("sk-", strings.Repeat("b", 30))

	in := `{"display":"rotate ` + key1 + `","timestamp":1,"project":"/p","sessionId":"s"}
{"display":"rotate ` + key2 + `","timestamp":1,"project":"/p","sessionId":"s"}
`

	es, _, _ := Parse("h", strings.NewReader(in), true)

	for _, e := range es {
		if strings.Contains(e.Display, "sk-") {
			t.Errorf("a key reached the store: %q", e.Display)
		}
	}

	if es[0].Display != es[1].Display {
		t.Fatal("test setup: both should redact to the same text")
	}

	if es[0].Fp == es[1].Fp {
		t.Error("two different prompts that redact identically must still be distinct entries")
	}
}
