package gitx

import (
	"context"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/transport"
)

// stubHost answers git commands from a table of substring -> result.
type stubHost struct {
	mu       sync.Mutex
	commands []string
	answers  map[string]transport.Result

	// headSequence, when set, returns a different HEAD on each rev-parse —
	// so a refresh can be seen to move the checkout.
	headSequence *[]string
}

func newStub() *stubHost {
	return &stubHost{answers: map[string]transport.Result{
		"'rev-parse' '--is-inside-work-tree'":    {Stdout: "true\n"},
		"'rev-parse' 'HEAD'":                     {Stdout: "aaaaaaaaaaaa\n"},
		"'status' '--porcelain'":                 {},
		"'diff' '--name-only' '--diff-filter=U'": {},
		"'fetch' '--prune'":                      {},
	}}
}

func (s *stubHost) Run(_ context.Context, cmd string) (transport.Result, error) {
	s.mu.Lock()
	s.commands = append(s.commands, cmd)

	if s.headSequence != nil && strings.Contains(cmd, "'rev-parse' 'HEAD'") {
		head := (*s.headSequence)[0]
		if len(*s.headSequence) > 1 {
			*s.headSequence = (*s.headSequence)[1:]
		}
		s.mu.Unlock()

		return transport.Result{Stdout: head + "\n"}, nil
	}

	s.mu.Unlock()

	// Longest match wins, so a specific answer beats a general one.
	best, bestLen := transport.Result{}, -1

	for frag, res := range s.answers {
		if strings.Contains(cmd, frag) && len(frag) > bestLen {
			best, bestLen = res, len(frag)
		}
	}

	return best, nil
}

func (s *stubHost) RunStdin(ctx context.Context, cmd string, _ io.Reader) (transport.Result, error) {
	return s.Run(ctx, cmd)
}

func (s *stubHost) WriteFile(context.Context, string, fs.FileMode, io.Reader) error { return nil }
func (s *stubHost) Close() error                                                    { return nil }

func (s *stubHost) ran(frag string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, c := range s.commands {
		if strings.Contains(c, frag) {
			return true
		}
	}

	return false
}

func path() *config.Path {
	return &config.Path{
		Name: "notes-svc", Dir: "/srv/notes-svc",
		Remote: "origin", Branch: "main", Refresh: config.StrategyFFOnly,
	}
}

// Refresh is fetch + an explicit merge against <remote>/<branch>, not
// `git pull` — so it does not depend on upstream tracking (SPEC.md §10).
func TestRefreshUsesExplicitMergeNotPull(t *testing.T) {
	host := newStub()

	if _, err := Refresh(context.Background(), host, path(), config.StrategyFFOnly, false); err != nil {
		t.Fatal(err)
	}

	if host.ran("'pull'") {
		t.Error("refresh must not use git pull")
	}
	if !host.ran("'fetch' '--prune' 'origin'") {
		t.Error("refresh should fetch with --prune")
	}
	if !host.ran("'merge' '--ff-only' 'origin/main'") {
		t.Error("ff-only should merge against origin/main explicitly")
	}
}

func TestRefreshStrategies(t *testing.T) {
	cases := map[config.Strategy]string{
		config.StrategyFFOnly: "'merge' '--ff-only' 'origin/main'",
		config.StrategyRebase: "'rebase' 'origin/main'",
		config.StrategyMerge:  "'merge' '--no-edit' 'origin/main'",
	}

	for strategy, want := range cases {
		host := newStub()

		if _, err := Refresh(context.Background(), host, path(), strategy, false); err != nil {
			t.Fatalf("%s: %v", strategy, err)
		}

		if !host.ran(want) {
			t.Errorf("%s did not run %s", strategy, want)
		}
	}
}

func TestRefreshFetchOnlyStopsAfterFetch(t *testing.T) {
	host := newStub()

	if _, err := Refresh(context.Background(), host, path(), config.StrategyFetchOnly, false); err != nil {
		t.Fatal(err)
	}

	if !host.ran("'fetch'") {
		t.Error("fetch-only should still fetch")
	}
	if host.ran("'merge'") || host.ran("'rebase'") {
		t.Error("fetch-only must not merge or rebase")
	}
}

func TestRefreshRefusesDirtyCheckout(t *testing.T) {
	host := newStub()
	host.answers["'status' '--porcelain'"] = transport.Result{Stdout: " M app/tax.go\n"}

	_, err := Refresh(context.Background(), host, path(), config.StrategyFFOnly, false)
	if err == nil {
		t.Fatal("a dirty checkout should be refused")
	}

	if host.ran("'merge'") {
		t.Error("nothing should have been merged")
	}
}

func TestRefreshForceOverridesDirty(t *testing.T) {
	host := newStub()
	host.answers["'status' '--porcelain'"] = transport.Result{Stdout: " M app/tax.go\n"}

	result, err := Refresh(context.Background(), host, path(), config.StrategyFFOnly, true)
	if err != nil {
		t.Fatal(err)
	}

	if !result.WasDirty {
		t.Error("result should record that the checkout was dirty")
	}
	if !host.ran("'merge'") {
		t.Error("--force should proceed with the merge")
	}
}

func TestRefreshAllowDirtyOnThePath(t *testing.T) {
	host := newStub()
	host.answers["'status' '--porcelain'"] = transport.Result{Stdout: " M x\n"}

	p := path()
	p.AllowDirty = true

	if _, err := Refresh(context.Background(), host, p, config.StrategyFFOnly, false); err != nil {
		t.Fatalf("allow_dirty should permit the refresh: %v", err)
	}
}

// A failed merge is left mid-operation on purpose; the result names the
// conflicted paths instead of aborting on the operator's behalf.
func TestRefreshReportsConflictsWithoutAborting(t *testing.T) {
	host := newStub()
	host.answers["'merge' '--ff-only'"] = transport.Result{ExitCode: 1, Stderr: "fatal: Not possible to fast-forward"}
	host.answers["'diff' '--name-only' '--diff-filter=U'"] = transport.Result{Stdout: "app/tax.go\napp/vat.go\n"}

	result, err := Refresh(context.Background(), host, path(), config.StrategyFFOnly, false)
	if err == nil {
		t.Fatal("a conflict should be an error")
	}

	if !result.Conflict || len(result.Conflicts) != 2 {
		t.Errorf("conflicts not reported: %+v", result)
	}
	if host.ran("--abort") {
		t.Error("refresh must never abort on the operator's behalf")
	}
}

func TestRefreshReportsNoChangeWhenHeadIsUnmoved(t *testing.T) {
	result, err := Refresh(context.Background(), newStub(), path(), config.StrategyFFOnly, false)
	if err != nil {
		t.Fatal(err)
	}

	if result.Changed {
		t.Error("identical SHAs should report no change")
	}
	if result.Before != "aaaaaaaaaaaa" || result.After != "aaaaaaaaaaaa" {
		t.Errorf("before/after = %q/%q", result.Before, result.After)
	}
}

// When HEAD moves, the result carries both SHAs and a diffstat — enough to
// see what a refresh did without re-running anything.
func TestRefreshReportsChangeAndDiffstat(t *testing.T) {
	host := newStub()

	heads := []string{"beforesha1234", "aftersha56789"}
	host.headSequence = &heads

	host.answers["'diff' '--stat'"] = transport.Result{
		Stdout: " app/tax.go | 12 ++++++------\n 1 file changed, 6 insertions(+), 6 deletions(-)\n",
	}

	result, err := Refresh(context.Background(), host, path(), config.StrategyFFOnly, false)
	if err != nil {
		t.Fatal(err)
	}

	if !result.Changed {
		t.Error("a moved HEAD should report changed")
	}
	if result.Before != "beforesha1234" || result.After != "aftersha56789" {
		t.Errorf("before/after = %q/%q", result.Before, result.After)
	}
	if !strings.Contains(result.DiffStat, "1 file changed") {
		t.Errorf("diffstat = %q", result.DiffStat)
	}
	if !host.ran("'diff' '--stat' 'beforesha1234' 'aftersha56789'") {
		t.Error("diffstat should be computed between the two SHAs")
	}
}

func TestRefreshRejectsNonRepo(t *testing.T) {
	host := newStub()
	host.answers["'rev-parse' '--is-inside-work-tree'"] = transport.Result{ExitCode: 128, Stderr: "not a git repository"}

	if _, err := Refresh(context.Background(), host, path(), config.StrategyFFOnly, false); err == nil {
		t.Fatal("a non-repo should be refused")
	}
}

func TestAddWorktreeUsesResetBranch(t *testing.T) {
	host := newStub()
	host.answers["test -e"] = transport.Result{ExitCode: 1}

	if err := AddWorktree(context.Background(), host,
		"/srv/notes-svc", "/srv/wt/notes-svc/fix", "agent/fix", "origin", "main"); err != nil {
		t.Fatal(err)
	}

	// -B resets an existing branch rather than failing, which is what makes
	// relaunching a task id idempotent.
	if !host.ran("'worktree' 'add' '-B' 'agent/fix' '/srv/wt/notes-svc/fix' 'origin/main'") {
		t.Errorf("worktree add command wrong: %v", host.commands)
	}
}

// An existing worktree holds a previous run's work — reuse it, never clobber.
func TestAddWorktreeReusesExisting(t *testing.T) {
	host := newStub()
	host.answers["test -e"] = transport.Result{}

	if err := AddWorktree(context.Background(), host,
		"/srv/notes-svc", "/srv/wt/notes-svc/fix", "agent/fix", "origin", "main"); err != nil {
		t.Fatal(err)
	}

	if host.ran("'worktree' 'add'") {
		t.Error("an existing worktree should be reused, not recreated")
	}
}

func TestGitCommandsQuoteEveryArgument(t *testing.T) {
	got := git("/srv/it's here", "merge", "--ff-only", "origin/main")

	want := `git -C '/srv/it'\''s here' 'merge' '--ff-only' 'origin/main'`
	if got != want {
		t.Errorf("git() = %q, want %q", got, want)
	}
}

// An untracked file must not block a refresh: git pull tolerates them, and
// almost every real checkout has stray build output.
func TestRefreshIgnoresUntrackedFiles(t *testing.T) {
	host := newStub()

	if _, err := Refresh(context.Background(), host, path(), config.StrategyFFOnly, false); err != nil {
		t.Fatal(err)
	}

	if !host.ran("'--untracked-files=no'") {
		t.Errorf("the dirty guard must ask git to ignore untracked files: %v", host.commands)
	}
}

// cmdRecorder records the command a helper builds and succeeds.
type cmdRecorder struct{ into *string }

func (c cmdRecorder) Run(_ context.Context, cmd string) (transport.Result, error) {
	*c.into = cmd

	return transport.Result{}, nil
}

func (c cmdRecorder) RunStdin(ctx context.Context, cmd string, _ io.Reader) (transport.Result, error) {
	return c.Run(ctx, cmd)
}
func (cmdRecorder) WriteFile(context.Context, string, fs.FileMode, io.Reader) error { return nil }
func (cmdRecorder) Close() error                                                    { return nil }
