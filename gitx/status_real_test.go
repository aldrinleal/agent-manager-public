package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Runs the real sweep command through a real sh against real git, because the
// string tests above only prove the parser agrees with my idea of the format.
func TestSweepAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}

	root := t.TempDir()

	sh := func(dir, script string) {
		t.Helper()

		c := exec.Command("sh", "-c", script)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")

		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
	}

	origin := filepath.Join(root, "origin.git")
	sh(root, "git init -q --bare -b master origin.git")

	clone := func(name string) string {
		d := filepath.Join(root, name)
		sh(root, "git clone -q origin.git '"+name+"' 2>/dev/null || true")
		sh(d, "git checkout -q -B master 2>/dev/null || true")

		return d
	}

	seed := clone("seed")
	sh(seed, "echo one > f.txt && git add f.txt && git commit -qm one && git push -q -u origin master")

	clean := clone("clean")
	dirty := clone("dirty dir") // a space in the name, deliberately
	untracked := clone("untracked")
	merging := clone("merging")
	ahead := clone("ahead")

	sh(dirty, "echo changed >> f.txt")
	sh(untracked, "echo x > new1.txt && echo y > new2.txt")
	sh(ahead, "echo more > g.txt && git add g.txt && git commit -qm local")

	// A merge that stops on a conflict.
	sh(merging, "git checkout -q -b side && echo side > f.txt && git commit -qam side && git checkout -q master && echo main > f.txt && git commit -qam main && (git merge side || true)")

	missing := filepath.Join(root, "does-not-exist")

	paths := []SweepPath{
		{"clean", clean}, {"dirty", dirty}, {"untracked", untracked},
		{"merging", merging}, {"ahead", ahead}, {"missing", missing},
	}

	out, err := exec.Command("sh", "-c", StatusSweepCommand(paths)).CombinedOutput()
	if err != nil {
		t.Fatalf("sweep failed: %v\n%s", err, out)
	}

	got := map[string]SweepEntry{}
	for _, e := range ParseStatusSweep(string(out)) {
		got[e.Name] = e
	}

	if len(got) != len(paths) {
		t.Fatalf("parsed %d of %d paths:\n%s", len(got), len(paths), out)
	}

	if s := got["clean"].Status; s == nil || !s.Clean() || s.Branch != "master" || s.Upstream != "origin/master" {
		t.Errorf("clean: %+v / %q", got["clean"].Status, got["clean"].Err)
	}

	if s := got["dirty"].Status; s == nil || len(s.Dirty) != 1 || s.Dirty[0] != "f.txt" {
		t.Errorf("dirty: %+v / %q", s, got["dirty"].Err)
	}

	if s := got["untracked"].Status; s == nil || s.Untracked != 2 || len(s.Dirty) != 0 || !s.Clean() {
		t.Errorf("untracked files must be counted but not make it dirty: %+v", s)
	}

	if s := got["ahead"].Status; s == nil || s.Ahead != 1 || s.Behind != 0 {
		t.Errorf("ahead: %+v", s)
	}

	s := got["merging"].Status
	if s == nil || s.InProgress != "merge" || s.Clean() {
		t.Errorf("a conflicted merge must be reported in-progress: %+v", s)
	}

	if s != nil && (len(s.Dirty) == 0 || !strings.Contains(strings.Join(s.Dirty, ","), "f.txt")) {
		t.Errorf("the unmerged file must be listed dirty: %v", s.Dirty)
	}

	if e := got["missing"]; e.Status != nil || e.Err == "" {
		t.Errorf("a missing path must be an error on its own, not fail the sweep: %+v", e)
	}

	_ = origin
}
