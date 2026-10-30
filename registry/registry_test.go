package registry

import (
	"context"
	"database/sql"
	"github.com/aldrinleal/agent-manager-public/history"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Registry {
	t.Helper()

	r, err := Open(filepath.Join(t.TempDir(), "agentmgr.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = r.Close() })

	return r
}

func sample(task string) *Session {
	return &Session{
		ID:          "id-" + task,
		TaskID:      task,
		Runner:      "claude-code",
		Host:        "homelab",
		RepoName:    "notes-svc",
		RepoDir:     "/home/dev/src/notes-svc",
		Worktree:    "/srv/agent-workspaces/notes-svc/" + task,
		Branch:      "agent/" + task,
		TmuxSession: "agent-notes-svc-" + task,
		LogPath:     "/srv/agent-logs/notes-svc/" + task + ".log",
		State:       StateStarting,
	}
}

func TestCreateGetRoundTrip(t *testing.T) {
	r := open(t)
	ctx := context.Background()

	in := sample("fix-tax")
	in.PromptPreview = "fix the rounding"

	if err := r.Create(ctx, in); err != nil {
		t.Fatal(err)
	}

	out, err := r.Get(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}

	if out.TaskID != in.TaskID || out.Host != in.Host || out.Branch != in.Branch {
		t.Errorf("round trip lost fields: %+v", out)
	}
	if out.State != StateStarting {
		t.Errorf("state = %q, want starting", out.State)
	}
	if out.PromptPreview != "fix the rounding" {
		t.Errorf("prompt preview = %q", out.PromptPreview)
	}
	// Nothing has set these yet; they must come back as nil, not zero times.
	if out.EndedAt != nil || out.ExitStatus != nil {
		t.Errorf("unset nullable fields came back non-nil: %+v", out)
	}
}

func TestUpdatePersistsExitStatusAndURL(t *testing.T) {
	r := open(t)
	ctx := context.Background()

	s := sample("bump")
	if err := r.Create(ctx, s); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	status := 0

	s.RemoteURL = "https://claude.ai/code/session_abc"
	s.State = StateStopped
	s.ExitStatus = &status
	s.EndedAt = &now

	if err := r.Update(ctx, s); err != nil {
		t.Fatal(err)
	}

	out, err := r.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}

	if out.RemoteURL != s.RemoteURL {
		t.Errorf("url = %q", out.RemoteURL)
	}
	// Exit status 0 is meaningful and must survive as a pointer, not be
	// flattened into "unset".
	if out.ExitStatus == nil || *out.ExitStatus != 0 {
		t.Errorf("exit status = %v, want 0", out.ExitStatus)
	}
	if out.EndedAt == nil {
		t.Error("ended_at was lost")
	}
}

func TestCreateRejectsDuplicateTask(t *testing.T) {
	r := open(t)
	ctx := context.Background()

	if err := r.Create(ctx, sample("dup")); err != nil {
		t.Fatal(err)
	}

	second := sample("dup")
	second.ID = "different-id"

	err := r.Create(ctx, second)
	if err == nil {
		t.Fatal("expected a conflict on (host, repo, task_id)")
	}
}

func TestListFilters(t *testing.T) {
	r := open(t)
	ctx := context.Background()

	running := sample("a")
	running.State = StateRunning

	stopped := sample("b")
	stopped.State = StateStopped

	other := sample("c")
	other.Host = "devbox"
	other.State = StateRunning

	for _, s := range []*Session{running, stopped, other} {
		if err := r.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	all, _ := r.List(ctx, Filter{})
	if len(all) != 3 {
		t.Errorf("got %d sessions, want 3", len(all))
	}

	byHost, _ := r.List(ctx, Filter{Host: "homelab"})
	if len(byHost) != 2 {
		t.Errorf("host filter got %d, want 2", len(byHost))
	}

	active, _ := r.List(ctx, Filter{Active: true})
	if len(active) != 2 {
		t.Errorf("active filter got %d, want 2", len(active))
	}

	byState, _ := r.List(ctx, Filter{State: StateStopped})
	if len(byState) != 1 {
		t.Errorf("state filter got %d, want 1", len(byState))
	}
}

// Nobody types a uuid at a terminal, so a task id has to resolve — but only
// when it is unambiguous.
func TestLookupByTaskID(t *testing.T) {
	r := open(t)
	ctx := context.Background()

	s := sample("only")
	if err := r.Create(ctx, s); err != nil {
		t.Fatal(err)
	}

	got, err := r.Lookup(ctx, "only")
	if err != nil {
		t.Fatal(err)
	}

	if got.ID != s.ID {
		t.Errorf("lookup by task id returned %q", got.ID)
	}

	if _, err := r.Lookup(ctx, "nope"); err == nil {
		t.Error("expected not-found for an unknown ref")
	}
}

func TestLookupPrefersTheActiveMatch(t *testing.T) {
	r := open(t)
	ctx := context.Background()

	// Same task id on two hosts: one finished, one still going.
	old := sample("shared")
	old.State = StateStopped

	live := sample("shared")
	live.ID = "id-live"
	live.Host = "devbox"
	live.State = StateRunning

	for _, s := range []*Session{old, live} {
		if err := r.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	got, err := r.Lookup(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}

	if got.ID != live.ID {
		t.Errorf("lookup returned %q, want the active session %q", got.ID, live.ID)
	}
}

func TestLookupAmbiguousIsAnError(t *testing.T) {
	r := open(t)
	ctx := context.Background()

	a := sample("shared")
	a.State = StateRunning

	b := sample("shared")
	b.ID = "id-b"
	b.Host = "devbox"
	b.State = StateRunning

	for _, s := range []*Session{a, b} {
		if err := r.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := r.Lookup(ctx, "shared"); err == nil {
		t.Fatal("two active matches should be an error, not a silent pick")
	}
}

func TestHostChecks(t *testing.T) {
	r := open(t)
	ctx := context.Background()

	missing, err := r.HostCheck(ctx, "nobody")
	if err != nil || missing != nil {
		t.Fatalf("cache miss should be (nil, nil), got (%v, %v)", missing, err)
	}

	if err := r.PutHostCheck(ctx, &HostCheck{
		Host: "homelab", OK: true, OS: "Linux", TmuxVersion: "tmux 3.4", GitVersion: "git version 2.43.0",
	}); err != nil {
		t.Fatal(err)
	}

	// Upsert, not a second row.
	if err := r.PutHostCheck(ctx, &HostCheck{Host: "homelab", OK: false, Note: "tmux not found"}); err != nil {
		t.Fatal(err)
	}

	got, err := r.HostCheck(ctx, "homelab")
	if err != nil {
		t.Fatal(err)
	}

	if got.OK || got.Note != "tmux not found" {
		t.Errorf("host check not upserted: %+v", got)
	}
	if got.CheckedAt.IsZero() {
		t.Error("checked_at was not set")
	}
}

func TestStateActive(t *testing.T) {
	for _, s := range []State{StateStarting, StateRunning, StateStale} {
		if !s.Active() {
			t.Errorf("%q should be active", s)
		}
	}

	for _, s := range []State{StateStopped, StateFailed} {
		if s.Active() {
			t.Errorf("%q should not be active", s)
		}
	}
}

// The deployed database lives on a PVC that outlasts every image, so a column
// added to the schema string only reaches fresh installs unless Open migrates.
// This builds the PRE-auth schema by hand and opens it.
func TestOpenMigratesAnOlderHostChecksTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = old.Exec(`CREATE TABLE host_checks (
		host TEXT PRIMARY KEY, ok INTEGER NOT NULL, os TEXT, tmux_version TEXT,
		git_version TEXT, runner_version TEXT, note TEXT, checked_at TEXT NOT NULL);
		INSERT INTO host_checks VALUES ('laptop-a',1,'Linux','tmux 3.7','git 2.5','2.1','', '2026-10-01T00:00:00Z');`)
	if err != nil {
		t.Fatal(err)
	}

	_ = old.Close()

	r, err := Open(path)
	if err != nil {
		t.Fatalf("opening an old database must migrate it, not fail: %v", err)
	}

	defer func() { _ = r.Close() }()

	got, err := r.HostCheck(context.Background(), "laptop-a")
	if err != nil || got == nil {
		t.Fatalf("existing row lost: %v %v", got, err)
	}

	if got.OS != "Linux" || got.AuthLoggedIn != nil {
		t.Errorf("an old row must read back intact with auth UNKNOWN (nil), not false: %+v", got)
	}

	// And a second Open is a no-op, not a "duplicate column" error.
	_ = r.Close()

	r2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}

	_ = r2.Close()
}

func TestHostCheckAuthIsTriState(t *testing.T) {
	r := open(t)
	ctx := context.Background()

	yes, no := true, false

	for name, v := range map[string]*bool{"unknown": nil, "in": &yes, "out": &no} {
		if err := r.PutHostCheck(ctx, &HostCheck{Host: name, OK: true, AuthLoggedIn: v, AuthEmail: "a@b", AuthSubscription: "max"}); err != nil {
			t.Fatal(err)
		}

		got, _ := r.HostCheck(ctx, name)

		switch {
		case v == nil && got.AuthLoggedIn != nil:
			t.Errorf("%s: nil must survive as nil, got %v", name, *got.AuthLoggedIn)
		case v != nil && (got.AuthLoggedIn == nil || *got.AuthLoggedIn != *v):
			t.Errorf("%s: want %v got %v", name, *v, got.AuthLoggedIn)
		}

		if got.AuthEmail != "a@b" || got.AuthSubscription != "max" {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestHistoryKeepsDistinctPromptsInTheSameMillisecondAndCollapsesIdenticalOnes(t *testing.T) {
	r := open(t)
	ctx := context.Background()

	es := []history.Entry{
		{Host: "laptop-a", SessionID: "s", TS: 1, Project: "/p", Display: "/exit ", Fp: "aaaa"},
		{Host: "laptop-a", SessionID: "s", TS: 1, Project: "/p", Display: "/exit ", Fp: "aaaa"}, // identical
		{Host: "laptop-a", SessionID: "s", TS: 2, Project: "/p", Display: "one", Fp: "bbbb"},
		{Host: "laptop-a", SessionID: "s", TS: 2, Project: "/p", Display: "two", Fp: "cccc"}, // same ms, different
	}

	n, err := r.InsertHistory(ctx, es)
	if err != nil || n != 3 {
		t.Fatalf("want 3 inserted (1 identical collapsed), got %d %v", n, err)
	}

	if again, _ := r.InsertHistory(ctx, es); again != 0 {
		t.Errorf("a second insert of the same entries must add 0, got %d", again)
	}
}

// history_entries first shipped keyed (host, session, ts). Opening that older
// table must rebuild it with the fingerprint key, not fail or keep losing rows.
func TestOpenRebuildsAnOlderHistoryTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	old, _ := sql.Open("sqlite", "file:"+path)
	_, err := old.Exec(`CREATE TABLE history_entries (host TEXT NOT NULL, session_id TEXT NOT NULL, ts INTEGER NOT NULL,
		project TEXT NOT NULL, display TEXT, PRIMARY KEY (host, session_id, ts));
		INSERT INTO history_entries VALUES ('laptop-a','s',1,'/p','old row');`)
	if err != nil {
		t.Fatal(err)
	}

	_ = old.Close()

	r, err := Open(path)
	if err != nil {
		t.Fatalf("opening the older table must migrate it: %v", err)
	}

	defer func() { _ = r.Close() }()

	n, err := r.InsertHistory(context.Background(), []history.Entry{
		{Host: "laptop-a", SessionID: "s", TS: 1, Project: "/p", Fp: "aaaa"},
		{Host: "laptop-a", SessionID: "s", TS: 1, Project: "/p", Fp: "bbbb"},
	})
	if err != nil || n != 2 {
		t.Errorf("the rebuilt table must key on the fingerprint: inserted %d, err %v", n, err)
	}
}

// Rows stored before redaction existed may hold credentials as written. Open
// discards them once, so the next pull re-ingests them through the redaction.
func TestOpenPurgesUnredactedHistoryOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the pre-redaction state: a row, and no marker.
	if _, err := r.InsertHistory(context.Background(), []history.Entry{{Host: "h", SessionID: "s", TS: 1, Project: "/p", Display: "token SECRET", Fp: "a"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := r.db.Exec(`DELETE FROM meta WHERE key = 'history_redacted_v1'`); err != nil {
		t.Fatal(err)
	}

	_ = r.Close()

	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	if n, _ := r2.HistoryCount(context.Background(), ""); n != 0 {
		t.Errorf("unredacted rows must be purged, %d remain", n)
	}

	// New rows (stored through the redaction) must then survive further opens.
	_, _ = r2.InsertHistory(context.Background(), []history.Entry{{Host: "h", SessionID: "s", TS: 2, Project: "/p", Display: "clean", Fp: "b"}})
	_ = r2.Close()

	r3, _ := Open(path)
	defer func() { _ = r3.Close() }()

	if n, _ := r3.HistoryCount(context.Background(), ""); n != 1 {
		t.Errorf("the purge must run ONCE; a later open wiped %d clean rows", 1-n)
	}
}
