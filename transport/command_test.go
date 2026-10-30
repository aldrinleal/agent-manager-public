package transport

import (
	"strings"
	"testing"
)

func TestWrapEnvIsSortedAndQuoted(t *testing.T) {
	got := WrapEnv(map[string]string{"PATH": "/a b:/c", "LANG": "C"}, "tmux ls")

	want := "export LANG='C'; export PATH='/a b:/c'; tmux ls"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	if WrapEnv(nil, "tmux ls") != "tmux ls" {
		t.Error("no env must leave the command alone")
	}
}

// The content never appears in the command — it goes down stdin.
func TestWriteFileCommandQuotesThePathOnly(t *testing.T) {
	got := WriteFileCommand("/srv/it's here/x.sh", 0o755)

	for _, part := range []string{`mkdir -p '/srv/it'\''s here'`, "chmod 0755", `mv -f '/srv/it'\''s here/x.sh.agentmgr.tmp' '/srv/it'\''s here/x.sh'`} {
		if !strings.Contains(got, part) {
			t.Errorf("%q missing %q", got, part)
		}
	}
}
