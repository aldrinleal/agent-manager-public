package history

import "regexp"

// redactions are the shapes of credentials that show up pasted into prompts.
// This is a safety net for storing prompt text, not a guarantee: a secret that
// matches none of these is stored as written. Each pattern errs toward matching
// real credentials (long, high-entropy, recognisable prefix) over catching every
// string that merely looks like one.
var redactions = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"private-key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)},
	{"anthropic-key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`)},
	{"api-key", regexp.MustCompile(`sk-(?:or-v1-)?[A-Za-z0-9_\-]{20,}`)},
	{"github-token", regexp.MustCompile(`\b(?:ghp|gho|ghs|ghu|ghr)_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}`)},
	{"aws-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`)},
}

// assignment catches `token: abc…`, `password=abc…`, `Authorization: Bearer abc…`:
// a credential-ish word followed by a long opaque value. Only the VALUE is
// replaced, so the prompt still reads sensibly.
var assignment = regexp.MustCompile(`(?i)((?:bearer|token|api[_-]?key|secret|password|passwd)["'\s:=]+)([A-Za-z0-9_\-/+=]{24,})`)

// Redact replaces anything that looks like a credential with a marker naming
// its kind, so a stored prompt is useful ("rotate this key") without being a
// second copy of the key.
func Redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, "[redacted:"+r.kind+"]")
	}

	return assignment.ReplaceAllString(s, "${1}[redacted:secret]")
}
