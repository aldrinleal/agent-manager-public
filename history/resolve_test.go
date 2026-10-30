package history

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Runs the real command through a real sh against real git. The 2026-09-22
// selection hit every one of these shapes by hand.
func TestResolveCommandAgainstRealGit(t *testing.T) {
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

	sh(root, "git init -q --bare -b trunk origin.git")
	sh(root, "git clone -q origin.git seed 2>/dev/null; cd seed && git checkout -q -B trunk && echo x > f && mkdir -p pkg/sub && echo y > pkg/sub/g && git add . && git -c user.name=t -c user.email=t@t commit -qm init && git push -q -u origin trunk")
	sh(root, "git clone -q origin.git normal")                                                                                      // origin/HEAD -> trunk
	sh(root, "git init -q fresh && cd fresh && git remote add origin git@example.com:x/y.git")                                      // EMPTY: a remote, no commits
	sh(root, "git init -q noremote && cd noremote && echo z > f && git add . && git -c user.name=t -c user.email=t@t commit -qm c") // commits, no origin
	sh(root, "mkdir plain && echo n > plain/file")                                                                                  // a directory, not a repo

	paths := []string{
		filepath.Join(root, "normal"),
		filepath.Join(root, "normal/pkg/sub"), // a subdirectory of a repo
		filepath.Join(root, "fresh"),
		filepath.Join(root, "noremote"),
		filepath.Join(root, "plain"),
		filepath.Join(root, "does-not-exist"),
		filepath.Join(root, "it's got a quote"),
	}

	sh(root, "mkdir \"it's got a quote\"")

	out, err := exec.Command("sh", "-c", ResolveCommand(paths)).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	got := ParseResolutions(string(out))

	n := filepath.Join(root, "normal")

	if r := got[n]; !r.Exists || !r.IsGit || !r.HasCommits || r.DefaultBranch != "trunk" || r.Top != n {
		t.Errorf("normal: %+v (the default branch must come from origin/HEAD, not be assumed to be master)", r)
	}

	if r := got[filepath.Join(root, "normal/pkg/sub")]; !r.IsGit || r.Top != n {
		t.Errorf("a subdirectory must report its repo's toplevel: %+v", r)
	}

	if r := got[filepath.Join(root, "fresh")]; !r.IsGit || r.HasCommits {
		t.Errorf("an empty repo (remote, no commits) must say has_commits=false: %+v", r)
	}

	if r := got[filepath.Join(root, "noremote")]; !r.HasCommits || r.DefaultBranch != "" {
		t.Errorf("no origin/HEAD means an empty default branch: %+v", r)
	}

	if r := got[filepath.Join(root, "plain")]; !r.Exists || r.IsGit {
		t.Errorf("a plain directory: %+v", r)
	}

	if r, ok := got[filepath.Join(root, "does-not-exist")]; !ok || r.Exists {
		t.Errorf("a missing path must be reported as missing, not omitted: %+v ok=%v", r, ok)
	}

	if r := got[filepath.Join(root, "it's got a quote")]; !r.Exists {
		t.Errorf("a path containing a quote broke the command: %+v", r)
	}

	if len(got) != len(paths) {
		t.Errorf("got %d of %d paths:\n%s", len(got), len(paths), out)
	}
}

func TestResolveCommandSkipsPathsItCannotRepresent(t *testing.T) {
	if cmd := ResolveCommand([]string{"/a|b", "/c\nd"}); strings.TrimSpace(cmd) != "" {
		t.Errorf("a path with | or a newline would corrupt the output format: %q", cmd)
	}
}
