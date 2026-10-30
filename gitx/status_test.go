package gitx

import (
	"strings"
	"testing"
)

func TestParseStatusV2Full(t *testing.T) {
	out := strings.Join([]string{
		"# branch.oid f5a01b00aabbccddeeff00112233445566778899",
		"# branch.head master",
		"# branch.upstream origin/master",
		"# branch.ab +2 -5",
		"1 .M N... 100644 100644 100644 aaaa bbbb app/tax.go",
		"1 M. N... 100644 100644 100644 aaaa bbbb dir with space/file name.go",
		"2 R. N... 100644 100644 100644 aaaa bbbb R100 new/name.go\told/name.go",
		"u UU N... 100644 100644 100644 100644 aaaa bbbb cccc conflicted.go",
		"? scratch.txt",
		"? build/",
	}, "\n")

	st := ParseStatusV2(out)

	if st.Branch != "master" || st.Upstream != "origin/master" || st.Ahead != 2 || st.Behind != 5 {
		t.Errorf("branch info wrong: %+v", st)
	}

	if st.Head != "f5a01b00aabbccddeeff00112233445566778899" {
		t.Errorf("head = %q", st.Head)
	}

	want := []string{"app/tax.go", "dir with space/file name.go", "new/name.go", "conflicted.go"}
	if strings.Join(st.Dirty, "|") != strings.Join(want, "|") {
		t.Errorf("dirty = %v, want %v", st.Dirty, want)
	}

	if st.Untracked != 2 {
		t.Errorf("untracked = %d, want 2", st.Untracked)
	}
}

func TestParseStatusV2Detached(t *testing.T) {
	st := ParseStatusV2("# branch.oid abc123\n# branch.head (detached)\n")

	if st.Branch != "" || st.Upstream != "" || st.Ahead != 0 {
		t.Errorf("detached head should have no branch/upstream: %+v", st)
	}
}

// A brand-new repo has no commits: oid is "(initial)", and there is no upstream.
func TestParseStatusV2InitialCommit(t *testing.T) {
	st := ParseStatusV2("# branch.oid (initial)\n# branch.head main\n")

	if st.Head != "" || st.Branch != "main" {
		t.Errorf("initial repo parsed wrong: %+v", st)
	}
}

func TestParseStatusV2NoUpstreamMeansNoCounts(t *testing.T) {
	st := ParseStatusV2("# branch.oid abc\n# branch.head feature\n")

	if st.Upstream != "" || st.Ahead != 0 || st.Behind != 0 {
		t.Errorf("no upstream, no ahead/behind: %+v", st)
	}
}

// Dirty is a non-nil slice so JSON gives [] not null.
func TestParseStatusV2CleanHasEmptyDirty(t *testing.T) {
	st := ParseStatusV2("# branch.oid abc\n# branch.head master\n")

	if st.Dirty == nil || len(st.Dirty) != 0 {
		t.Errorf("dirty = %#v", st.Dirty)
	}

	if !st.Clean() {
		t.Error("should be clean")
	}
}

func TestCleanFalseWhenAnOperationIsInProgress(t *testing.T) {
	if (RepoStatus{InProgress: "rebase"}).Clean() {
		t.Error("a stopped rebase is not clean even with no dirty files")
	}
}

func TestParseStatusBlockSeparatesInProgress(t *testing.T) {
	st, err := parseStatusBlock("# branch.oid a\n# branch.head m\n" + inProgressMarker + "\nrebase\n")
	if err != nil || st.InProgress != "rebase" {
		t.Fatalf("got %+v, err %v", st, err)
	}

	st, err = parseStatusBlock("# branch.oid a\n# branch.head m\n" + inProgressMarker + "\n")
	if err != nil || st.InProgress != "" {
		t.Fatalf("nothing in progress should be empty: %+v %v", st, err)
	}
}

func TestParseStatusBlockSurfacesGitErrors(t *testing.T) {
	_, err := parseStatusBlock("fatal: not a git repository\n" + inProgressMarker + "\n")
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("want the git error, got %v", err)
	}

	if _, err := parseStatusBlock("sh: cd: /nope: No such file or directory\n"); err == nil {
		t.Error("a missing marker means cd/git failed")
	}
}

func TestSweepRoundTrip(t *testing.T) {
	out := "@@PATH a\n# branch.oid 1\n# branch.head master\n1 .M N... 100644 100644 100644 h h x.go\n" +
		inProgressMarker + "\n@@END\n" +
		"@@PATH b\n@@ERR missing or not a git repository: /nope\n@@END\n" +
		"@@PATH c\n# branch.oid 2\n# branch.head feat\n" + inProgressMarker + "\nmerge\n@@END\n"

	es := ParseStatusSweep(out)

	if len(es) != 3 {
		t.Fatalf("got %d entries: %+v", len(es), es)
	}

	if es[0].Name != "a" || es[0].Status == nil || len(es[0].Status.Dirty) != 1 {
		t.Errorf("a: %+v", es[0])
	}

	// One bad path must not poison the others.
	if es[1].Name != "b" || es[1].Status != nil || !strings.Contains(es[1].Err, "/nope") {
		t.Errorf("b: %+v", es[1])
	}

	if es[2].Status == nil || es[2].Status.InProgress != "merge" {
		t.Errorf("c: %+v", es[2])
	}
}

func TestSweepCommandIsOneCommandCoveringEveryPathQuoted(t *testing.T) {
	cmd := StatusSweepCommand([]SweepPath{{"a", "/srv/it's here"}, {"b", "/srv/b"}})

	for _, want := range []string{"@@PATH a", "@@PATH b", `'/srv/it'\''s here'`, "--porcelain=v2"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("sweep command lacks %q:\n%s", want, cmd)
		}
	}

	if strings.Count(cmd, "\n") != 0 {
		t.Error("the sweep must be a single line, so it is one shell command")
	}
}
