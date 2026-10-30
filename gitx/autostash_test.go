package gitx

import (
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/transport"
)

// localTransport runs commands through a real sh on THIS machine, so the git
// behaviour under test is real git's, not a stub's idea of it.
type localTransport struct{}

func (localTransport) Run(ctx context.Context, cmd string) (transport.Result, error) {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_EDITOR=true")

	var out, errb strings.Builder
	c.Stdout, c.Stderr = &out, &errb

	res := transport.Result{}
	err := c.Run()

	res.Stdout, res.Stderr = out.String(), errb.String()

	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}

	return res, err
}

func (l localTransport) RunStdin(ctx context.Context, cmd string, _ io.Reader) (transport.Result, error) {
	return l.Run(ctx, cmd)
}
func (localTransport) WriteFile(context.Context, string, fs.FileMode, io.Reader) error { return nil }
func (localTransport) Close() error                                                    { return nil }

// repoLab is an origin plus one working clone, with helpers.
type repoLab struct {
	t        *testing.T
	root     string
	origin   string
	work     string
	upstream string // a second clone used to push "upstream" commits
}

func newLab(t *testing.T) *repoLab {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}

	l := &repoLab{t: t, root: t.TempDir()}
	l.origin = filepath.Join(l.root, "origin.git")
	l.work = filepath.Join(l.root, "work")
	l.upstream = filepath.Join(l.root, "upstream")

	l.sh(l.root, "git init -q --bare -b master origin.git")
	l.sh(l.root, "git clone -q origin.git upstream 2>/dev/null; cd upstream && git checkout -q -B master")
	l.sh(l.upstream, "printf 'alpha\\nbeta\\ngamma\\n' > f.txt && printf 'one\\n' > g.txt && git add . && git commit -qm init && git push -q -u origin master")
	l.sh(l.root, "git clone -q origin.git work")

	return l
}

func (l *repoLab) sh(dir, script string) string {
	l.t.Helper()

	res, err := localTransport{}.Run(context.Background(), "cd '"+dir+"' && "+script)
	if err != nil || res.ExitCode != 0 {
		l.t.Fatalf("%s: exit %d %v\n%s%s", script, res.ExitCode, err, res.Stdout, res.Stderr)
	}

	return res.Stdout
}

// pushUpstream makes a commit in the "someone else" clone and pushes it.
func (l *repoLab) pushUpstream(script string) {
	l.t.Helper()
	l.sh(l.upstream, script+" && git commit -qam upstream && git push -q origin master")
}

func (l *repoLab) path() *config.Path {
	return &config.Path{Name: "lab", Dir: l.work, Remote: "origin", Branch: "master", Refresh: config.StrategyFFOnly}
}

func (l *repoLab) refresh(o RefreshOpts) (*RefreshResult, error) {
	return RefreshWith(context.Background(), localTransport{}, l.path(), o)
}

func (l *repoLab) read(file string) string {
	b, err := os.ReadFile(filepath.Join(l.work, file))
	if err != nil {
		l.t.Fatal(err)
	}

	return string(b)
}

func TestAutostashRebaseRestoresUncommittedChanges(t *testing.T) {
	l := newLab(t)

	l.pushUpstream("printf 'two\\n' >> g.txt") // upstream touches g.txt
	l.sh(l.work, "printf 'LOCAL\\n' >> f.txt") // local, uncommitted, a DIFFERENT file

	before := l.sh(l.work, "git diff")

	res, err := l.refresh(RefreshOpts{Strategy: config.StrategyRebase, Autostash: true})
	if err != nil {
		t.Fatalf("refresh: %v\n%+v", err, res)
	}

	if !res.Changed || !res.Autostash || res.Stashed != 1 {
		t.Errorf("result: %+v", res)
	}

	if res.StashRef != "" {
		t.Errorf("a clean autostash must leave no stash behind, got %s", res.StashRef)
	}

	// The handover's acceptance check: the modification is back, exactly.
	if after := l.sh(l.work, "git diff"); after != before {
		t.Errorf("uncommitted diff changed across the refresh:\n--- before\n%s\n--- after\n%s", before, after)
	}

	if !strings.Contains(l.read("g.txt"), "two") {
		t.Error("the upstream change was not brought in")
	}

	if n := strings.TrimSpace(l.sh(l.work, "git stash list | wc -l")); n != "0" {
		t.Errorf("stash should be empty afterwards, has %s entries", n)
	}
}

func TestAutostashMergeAlsoWorks(t *testing.T) {
	l := newLab(t)

	l.pushUpstream("printf 'two\\n' >> g.txt")
	l.sh(l.work, "printf 'LOCAL\\n' >> f.txt && git -c user.name=t -c user.email=t@t commit -qam localcommit && printf 'DIRTY\\n' >> f.txt")

	res, err := l.refresh(RefreshOpts{Strategy: config.StrategyMerge, Autostash: true})
	if err != nil {
		t.Fatalf("%v\n%+v", err, res)
	}

	if !strings.Contains(l.read("f.txt"), "DIRTY") || !strings.Contains(l.read("g.txt"), "two") {
		t.Errorf("merge+autostash lost something:\nf=%q g=%q", l.read("f.txt"), l.read("g.txt"))
	}
}

// The case the whole "never drop" rule exists for: the dirty change collides
// with the incoming one, so re-applying the stash conflicts. Git keeps the
// entry; we must report it, and must not have removed it.
func TestAutostashConflictKeepsTheStashAndReportsIt(t *testing.T) {
	l := newLab(t)

	l.pushUpstream("sed -i 's/beta/BETA-UPSTREAM/' f.txt")
	l.sh(l.work, "sed -i 's/beta/beta-LOCAL/' f.txt") // same line, uncommitted

	res, err := l.refresh(RefreshOpts{Strategy: config.StrategyRebase, Autostash: true})
	if err != nil {
		t.Fatalf("refresh should report, not fail: %v", err)
	}

	if res.StashRef == "" {
		t.Fatalf("the surviving stash must be reported: %+v", res)
	}

	if !strings.Contains(res.Message, res.StashRef) {
		t.Errorf("the message must name the stash, got %q", res.Message)
	}

	// Actually still there — not dropped, and it holds the user's change.
	if n := strings.TrimSpace(l.sh(l.work, "git stash list | wc -l")); n != "1" {
		t.Errorf("the stash must still exist, list has %s", n)
	}

	if body := l.sh(l.work, "git stash show -p stash@{0}"); !strings.Contains(body, "beta-LOCAL") {
		t.Errorf("the stash does not hold the user's change:\n%s", body)
	}

	if !strings.Contains(l.read("f.txt"), "<<<<<<<") {
		t.Error("the conflict should be left in the working tree for a human")
	}
}

func TestRefreshRefusesWhileAnOperationIsStopped(t *testing.T) {
	l := newLab(t)

	l.sh(l.work, "git checkout -q -b side && sed -i 's/alpha/ALPHA-SIDE/' f.txt && git -c user.name=t -c user.email=t@t commit -qam side && git checkout -q master && sed -i 's/alpha/ALPHA-MAIN/' f.txt && git -c user.name=t -c user.email=t@t commit -qam main && (git merge side >/dev/null 2>&1 || true)")

	_, err := l.refresh(RefreshOpts{Strategy: config.StrategyFFOnly})
	if err == nil || !strings.Contains(err.Error(), "mid-merge") {
		t.Fatalf("want a refusal naming the stopped merge, got %v", err)
	}

	// And it must not have touched the repo further.
	if op, _ := InProgress(context.Background(), localTransport{}, l.work); op != "merge" {
		t.Errorf("the merge should still be waiting, got %q", op)
	}
}

func TestDirtyWithoutAutostashIsStillRefused(t *testing.T) {
	l := newLab(t)

	l.pushUpstream("printf 'two\\n' >> g.txt")
	l.sh(l.work, "printf 'LOCAL\\n' >> f.txt")

	if _, err := l.refresh(RefreshOpts{Strategy: config.StrategyRebase}); err == nil {
		t.Fatal("a dirty tree without autostash/force/allow_dirty must be refused")
	}
}

// ff-only is safe on a dirty tree, so autostash is accepted as permission to
// proceed — but git is not told to stash anything.
func TestFFOnlyWithAutostashProceedsWithoutStashing(t *testing.T) {
	l := newLab(t)

	l.pushUpstream("printf 'two\\n' >> g.txt")
	l.sh(l.work, "printf 'LOCAL\\n' >> f.txt")

	res, err := l.refresh(RefreshOpts{Strategy: config.StrategyFFOnly, Autostash: true})
	if err != nil {
		t.Fatalf("%v\n%+v", err, res)
	}

	if res.Autostash {
		t.Error("ff-only never stashes; the result must not claim it did")
	}

	if !res.Changed || !strings.Contains(l.read("f.txt"), "LOCAL") {
		t.Errorf("ff-only should update and keep the local change: %+v", res)
	}
}
