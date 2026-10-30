package history

import (
	"io"
	"strings"
	"testing"
)

const sample = `{"display":"fix the tax bug","pastedContents":{},"timestamp":1775149282519,"project":"/home/dev/src/api-kit","sessionId":"s1"}
{"display":"and add a test","pastedContents":{},"timestamp":1775149299999,"project":"/home/dev/src/api-kit","sessionId":"s1"}
not json at all
{"display":"no project","timestamp":1775149300000,"sessionId":"s2"}
{"display":"no timestamp","project":"/p","sessionId":"s3"}
{"display":"last line without a newline","timestamp":1775149400000,"project":"/home/dev/src/cli-kit","sessionId":"s4"}`

func TestParseCountsEverythingAndSkipsBadLines(t *testing.T) {
	es, st, err := Parse("laptop-a", strings.NewReader(sample), true)
	if err != nil {
		t.Fatal(err)
	}

	// 3 good + 3 bad, including a final line with no trailing newline.
	if len(es) != 3 || st.Entries != 3 || st.Bad != 3 || st.Lines != 6 {
		t.Fatalf("entries=%d stats=%+v", len(es), st)
	}

	if es[2].Project != "/home/dev/src/cli-kit" || es[2].Display != "last line without a newline" {
		t.Errorf("the last, unterminated line was lost: %+v", es[2])
	}

	if es[0].Host != "laptop-a" || es[0].TS != 1775149282519 || es[0].SessionID != "s1" {
		t.Errorf("%+v", es[0])
	}
}

// Privacy switch: with prompts off the text is never kept, but the entry is.
func TestParseDropsPromptTextWhenNotStoring(t *testing.T) {
	es, _, _ := Parse("h", strings.NewReader(sample), false)

	if len(es) != 3 {
		t.Fatalf("entries must still be recorded: %d", len(es))
	}

	for _, e := range es {
		if e.Display != "" {
			t.Errorf("prompt text kept with storePrompts=false: %q", e.Display)
		}
	}
}

// A record with a huge pasted payload must not stop the parse: a
// bufio.Scanner would hit its token limit there and silently drop the rest.
func TestParseSurvivesAnEnormousLine(t *testing.T) {
	huge := `{"display":"x","pastedContents":{"1":{"content":"` + strings.Repeat("A", 12<<20) + `"}},"timestamp":1775149282519,"project":"/p/a","sessionId":"s"}`

	es, st, err := Parse("h", strings.NewReader(huge+"\n"+sample+"\n"), true)
	if err != nil {
		t.Fatal(err)
	}

	if len(es) != 4 {
		t.Fatalf("got %d entries (%+v); the lines after the huge one were lost", len(es), st)
	}
}

func TestParseClipsLongPromptsOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", MaxDisplay) // 2 bytes each: a naive byte cut would split one

	es, _, _ := Parse("h", strings.NewReader(`{"display":"`+long+`","timestamp":1,"project":"/p","sessionId":"s"}`+"\n"), true)

	if len(es) != 1 {
		t.Fatal("no entry")
	}

	if !strings.HasSuffix(es[0].Display, "…") {
		t.Error("a clipped prompt should end with an ellipsis")
	}

	for _, r := range es[0].Display {
		if r == '�' {
			t.Fatal("clipping split a multi-byte rune")
		}
	}
}

// The parse reports a read error rather than pretending the file ended.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestParseSurfacesAReadError(t *testing.T) {
	if _, _, err := Parse("h", errReader{}, true); err == nil {
		t.Error("a failed read must be an error")
	}
}

func TestExcluder(t *testing.T) {
	e := Excluder{Substrings: DefaultExcludes}

	for p, want := range map[string]bool{
		"/home/dev":                      true, // the home directory itself
		"/home/dev/":                     true,
		"/root":                          true,
		"/home/dev/src/course-scratch-x": true, // case-insensitive "scratch"
		"/home/dev/.openclaw":            true,
		"/home/dev/.claude/plugins":      true,
		"/home/dev/src/api-kit":          false,
		"/home/dev/src/k8s-descriptors":  false,
		// "scratch" is a substring rule on purpose — but a home-adjacent path that
		// merely starts with the home dir is NOT the home dir.
		"/home/dev/src": false,
	} {
		if got, _ := e.Excluded(p); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestParseExcludes(t *testing.T) {
	got := ParseExcludes(" scratch , /.openclaw,, ")

	if len(got) != 2 || got[0] != "scratch" || got[1] != "/.openclaw" {
		t.Errorf("%v", got)
	}
}

// Real data from laptop-a: claude logs a repeated /exit three times in one
// millisecond (identical), and once two DIFFERENT prompts landed in the same
// millisecond. Identical must collapse; different must both survive.
func TestFingerprintSeparatesDistinctPromptsAndMergesIdenticalOnes(t *testing.T) {
	in := `{"display":"/exit ","timestamp":1761955410022,"project":"/p/a","sessionId":"s"}
{"display":"/exit ","timestamp":1761955410022,"project":"/p/a","sessionId":"s"}
{"display":"create a branch","timestamp":1762917260953,"project":"/p/a","sessionId":"s"}
{"display":"rebase against develop","timestamp":1762917260953,"project":"/p/a","sessionId":"s"}
`

	es, _, _ := Parse("h", strings.NewReader(in), true)

	if es[0].Fp != es[1].Fp {
		t.Error("identical entries must share a fingerprint so they collapse")
	}

	if es[2].Fp == es[3].Fp {
		t.Error("different prompts in the same millisecond must NOT share one: it would drop a real prompt")
	}

	// The fingerprint comes from the original text, so it is the same whether or
	// not prompts are being stored.
	off, _, _ := Parse("h", strings.NewReader(in), false)
	if off[2].Fp != es[2].Fp || off[2].Display != "" {
		t.Errorf("the fingerprint must not depend on the privacy switch: %q vs %q", off[2].Fp, es[2].Fp)
	}
}
