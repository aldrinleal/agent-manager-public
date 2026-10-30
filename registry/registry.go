// Package registry is agent-manager's system of record: every session it ever
// launched, and the cached preflight result for every host.
//
// need.md's conclusion, made concrete — no vendor exposes a way to enumerate
// remote sessions with their URLs, so the launcher records what it knows at
// launch time and reconciles liveness itself.
package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joomcode/errorx"

	// Pure-Go sqlite. NOT mattn/go-sqlite3: the Makefile builds with
	// CGO_ENABLED=0 (api-kit's recipe), and a cgo driver silently breaks it.
	_ "modernc.org/sqlite"
)

var Errors = errorx.NewNamespace("registry")

var (
	ErrDB       = Errors.NewType("db")
	ErrNotFound = Errors.NewType("not_found")
	ErrConflict = Errors.NewType("conflict")
)

// State is where a session is in its life.
type State string

const (
	// StateStarting — tmux session created, Remote Control URL not yet found.
	StateStarting State = "starting"
	// StateRunning — session exists and its pane is alive.
	StateRunning State = "running"
	// StateStale — alive, but the log has not grown for StaleAfter.
	StateStale State = "stale"
	// StateBlocked — alive and WAITING FOR A HUMAN: a permission or trust prompt
	// is on screen. Distinct from stale, which cannot tell "thinking" from
	// "waiting for you to approve a tool call" — and the second is the one that
	// needs someone to act.
	StateBlocked State = "blocked"

	// StateStopped — pane dead or session gone; ExitStatus is meaningful.
	StateStopped State = "stopped"
	// StateFailed — the launch itself errored; Note says which step.
	StateFailed State = "failed"
)

// Active reports whether a state is one the fleet should keep watching.
func (s State) Active() bool {
	return s == StateStarting || s == StateRunning || s == StateStale || s == StateBlocked
}

// Session is one registry row.
type Session struct {
	ID            string     `json:"id"`
	TaskID        string     `json:"task_id"`
	Runner        string     `json:"runner"`
	Host          string     `json:"host"`
	RepoName      string     `json:"repo_name"`
	RepoDir       string     `json:"repo_dir"`
	Worktree      string     `json:"worktree"`
	Branch        string     `json:"branch"`
	TmuxSession   string     `json:"tmux_session"`
	LogPath       string     `json:"log_path"`
	RemoteURL     string     `json:"remote_url,omitempty"`
	State         State      `json:"state"`
	PanePID       int        `json:"pane_pid,omitempty"`
	ExitStatus    *int       `json:"exit_status,omitempty"`
	PromptPreview string     `json:"prompt_preview,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	Note          string     `json:"note,omitempty"`
}

// HostCheck is a cached preflight result.
type HostCheck struct {
	Host          string    `json:"host"`
	OK            bool      `json:"ok"`
	OS            string    `json:"os,omitempty"`
	TmuxVersion   string    `json:"tmux_version,omitempty"`
	GitVersion    string    `json:"git_version,omitempty"`
	RunnerVersion string    `json:"runner_version,omitempty"`
	Note          string    `json:"note,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`

	// Auth is what `claude auth status` said. AuthLoggedIn is tri-state: nil
	// means "could not tell" (an older claude without `auth`, or unparseable
	// output), which is deliberately not the same as "logged out" — only a
	// definite false fails preflight.
	AuthLoggedIn     *bool  `json:"auth_logged_in"`
	AuthEmail        string `json:"auth_email,omitempty"`
	AuthSubscription string `json:"auth_subscription,omitempty"`
}

// Registry wraps the sqlite file.
type Registry struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  id              TEXT PRIMARY KEY,
  task_id         TEXT NOT NULL,
  runner          TEXT NOT NULL,
  host            TEXT NOT NULL,
  repo_name       TEXT NOT NULL,
  repo_dir        TEXT NOT NULL,
  worktree        TEXT NOT NULL,
  branch          TEXT NOT NULL,
  tmux_session    TEXT NOT NULL,
  log_path        TEXT NOT NULL,
  remote_url      TEXT,
  state           TEXT NOT NULL,
  pane_pid        INTEGER,
  exit_status     INTEGER,
  prompt_preview  TEXT,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  last_seen_at    TEXT,
  ended_at        TEXT,
  note            TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS sessions_key ON sessions(host, repo_name, task_id);
CREATE INDEX IF NOT EXISTS sessions_state ON sessions(state);

CREATE TABLE IF NOT EXISTS host_checks (
  host            TEXT PRIMARY KEY,
  ok              INTEGER NOT NULL,
  os              TEXT,
  tmux_version    TEXT,
  git_version     TEXT,
  runner_version  TEXT,
  note            TEXT,
  checked_at      TEXT NOT NULL,
  auth_logged_in  INTEGER,
  auth_email      TEXT,
  auth_subscription TEXT
);

CREATE TABLE IF NOT EXISTS history_entries (
  host        TEXT NOT NULL,
  session_id  TEXT NOT NULL,
  ts          INTEGER NOT NULL,
  fp          TEXT NOT NULL DEFAULT '',
  project     TEXT NOT NULL,
  display     TEXT,
  PRIMARY KEY (host, session_id, ts, fp)
);
CREATE INDEX IF NOT EXISTS history_project ON history_entries(project, ts);
CREATE INDEX IF NOT EXISTS history_ts ON history_entries(ts);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS history_pulls (
  host      TEXT PRIMARY KEY,
  pulled_at TEXT NOT NULL,
  bytes     INTEGER NOT NULL DEFAULT 0,
  parsed    INTEGER NOT NULL DEFAULT 0,
  inserted  INTEGER NOT NULL DEFAULT 0,
  bad       INTEGER NOT NULL DEFAULT 0,
  note      TEXT
);
`

// Open opens (and migrates) the registry at path.
func Open(path string) (*Registry, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, ErrDB.Wrap(err, "creating directory for %s", path)
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)",
		url.PathEscape(path))

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, ErrDB.Wrap(err, "opening %s", path)
	}

	// Personal-scale tool: one writer, no contention to manage. Same call
	// ../notes-svc made with its module-level lock.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, ErrDB.Wrap(err, "applying schema to %s", path)
	}

	r := &Registry{db: db}

	if err := r.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return r, nil
}

// migrate adds columns introduced after a database file was first created.
// CREATE TABLE IF NOT EXISTS does nothing to a table that already exists, and
// the deployed registry lives on a PVC that outlasts every image, so a new
// column in the schema string above would otherwise only ever reach fresh
// installs. Idempotent: each column is added only when missing.
func (r *Registry) migrate() error {
	for _, col := range []struct{ table, name, ddl string }{
		{"host_checks", "auth_logged_in", "INTEGER"},
		{"host_checks", "auth_email", "TEXT"},
		{"host_checks", "auth_subscription", "TEXT"},
	} {
		has, err := r.hasColumn(col.table, col.name)
		if err != nil {
			return err
		}

		if has {
			continue
		}

		if _, err := r.db.Exec("ALTER TABLE " + col.table + " ADD COLUMN " + col.name + " " + col.ddl); err != nil {
			return ErrDB.Wrap(err, "adding %s.%s", col.table, col.name)
		}
	}

	// history_entries first shipped keyed (host, session, ts), which collapsed
	// distinct prompts sharing a millisecond. SQLite cannot change a primary key
	// in place, so the table is rebuilt. Its contents are a cache of the hosts'
	// own files, re-read on the next pull, so dropping it loses nothing that is
	// not re-ingested — true only while the files still hold that history; do not
	// copy this pattern for data that exists nowhere else.
	if has, err := r.hasColumn("history_entries", "fp"); err != nil {
		return err
	} else if !has {
		if _, err := r.db.Exec(`DROP TABLE IF EXISTS history_entries; DROP INDEX IF EXISTS history_project; DROP INDEX IF EXISTS history_ts;`); err != nil {
			return ErrDB.Wrap(err, "rebuilding history_entries")
		}

		if _, err := r.db.Exec(schema); err != nil {
			return ErrDB.Wrap(err, "recreating history_entries")
		}
	}

	// Prompt text used to be stored as written, secrets included. It is redacted
	// on ingest now (history.Redact), but rows already stored were not. They are a
	// cache of the hosts' own files, so they are discarded once and re-ingested
	// through the redaction on the next pull.
	if done, err := r.metaGet("history_redacted_v1"); err != nil {
		return err
	} else if done == "" {
		if _, err := r.db.Exec(`DELETE FROM history_entries`); err != nil {
			return ErrDB.Wrap(err, "purging unredacted history")
		}

		if err := r.metaSet("history_redacted_v1", "1"); err != nil {
			return err
		}
	}

	return nil
}

func (r *Registry) metaGet(key string) (string, error) {
	var v string

	err := r.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	if err != nil {
		return "", ErrDB.Wrap(err, "reading meta %s", key)
	}

	return v, nil
}

func (r *Registry) metaSet(key, value string) error {
	if _, err := r.db.Exec(`INSERT INTO meta (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return ErrDB.Wrap(err, "writing meta %s", key)
	}

	return nil
}

func (r *Registry) hasColumn(table, column string) (bool, error) {
	rows, err := r.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, ErrDB.Wrap(err, "reading schema of %s", table)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid         int
			name, ctype string
			notnull, pk int
			dflt        any
		)

		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, ErrDB.Wrap(err, "reading schema of %s", table)
		}

		if name == column {
			return true, nil
		}
	}

	return false, rows.Err()
}

func (r *Registry) Close() error { return r.db.Close() }

const sessionColumns = `id, task_id, runner, host, repo_name, repo_dir, worktree, branch,
	tmux_session, log_path, remote_url, state, pane_pid, exit_status, prompt_preview,
	created_at, updated_at, last_seen_at, ended_at, note`

// Create inserts a new session. A duplicate (host, repo, task_id) is reported
// as a conflict so callers can offer the existing row instead.
func (r *Registry) Create(ctx context.Context, s *Session) error {
	now := time.Now().UTC()
	s.CreatedAt, s.UpdatedAt = now, now

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sessions (`+sessionColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.TaskID, s.Runner, s.Host, s.RepoName, s.RepoDir, s.Worktree, s.Branch,
		s.TmuxSession, s.LogPath, nullString(s.RemoteURL), string(s.State), s.PanePID,
		nullInt(s.ExitStatus), nullString(s.PromptPreview),
		ts(s.CreatedAt), ts(s.UpdatedAt), nullTime(s.LastSeenAt), nullTime(s.EndedAt),
		nullString(s.Note))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrConflict.New("a session for %s/%s task %q already exists", s.Host, s.RepoName, s.TaskID)
		}
		return ErrDB.Wrap(err, "inserting session %s", s.ID)
	}

	return nil
}

// Update writes a session row back.
func (r *Registry) Update(ctx context.Context, s *Session) error {
	s.UpdatedAt = time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		UPDATE sessions SET
			remote_url = ?, state = ?, pane_pid = ?, exit_status = ?,
			updated_at = ?, last_seen_at = ?, ended_at = ?, note = ?,
			branch = ?, worktree = ?, log_path = ?
		WHERE id = ?`,
		nullString(s.RemoteURL), string(s.State), s.PanePID, nullInt(s.ExitStatus),
		ts(s.UpdatedAt), nullTime(s.LastSeenAt), nullTime(s.EndedAt), nullString(s.Note),
		s.Branch, s.Worktree, s.LogPath,
		s.ID)
	if err != nil {
		return ErrDB.Wrap(err, "updating session %s", s.ID)
	}

	return nil
}

// Get fetches one session by id.
func (r *Registry) Get(ctx context.Context, id string) (*Session, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)

	s, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound.New("no session with id %q", id)
	}

	return s, err
}

// Lookup resolves a session by id, or — for the CLI, where nobody types a
// uuid — by task id. An ambiguous task id is an error naming the candidates,
// rather than a silent pick.
func (r *Registry) Lookup(ctx context.Context, ref string) (*Session, error) {
	if s, err := r.Get(ctx, ref); err == nil {
		return s, nil
	}

	matches, err := r.List(ctx, Filter{TaskID: ref})
	if err != nil {
		return nil, err
	}

	switch len(matches) {
	case 0:
		return nil, ErrNotFound.New("no session with id or task %q", ref)
	case 1:
		return matches[0], nil
	}

	// Prefer a single active match — relaunching a task id after the old one
	// stopped is normal, and that should not need disambiguating.
	var active []*Session
	for _, m := range matches {
		if m.State.Active() {
			active = append(active, m)
		}
	}

	if len(active) == 1 {
		return active[0], nil
	}

	var where []string
	for _, m := range matches {
		where = append(where, fmt.Sprintf("%s (%s/%s, %s)", m.ID, m.Host, m.RepoName, m.State))
	}

	return nil, ErrConflict.New("task %q matches %d sessions — use an id: %s",
		ref, len(matches), strings.Join(where, "; "))
}

// Filter narrows a List.
type Filter struct {
	Host     string
	RepoName string
	TaskID   string
	State    State
	Active   bool
	Limit    int
}

// List returns sessions newest first.
func (r *Registry) List(ctx context.Context, f Filter) ([]*Session, error) {
	query := `SELECT ` + sessionColumns + ` FROM sessions`

	var (
		clauses []string
		args    []any
	)

	if f.Host != "" {
		clauses = append(clauses, "host = ?")
		args = append(args, f.Host)
	}
	if f.RepoName != "" {
		clauses = append(clauses, "repo_name = ?")
		args = append(args, f.RepoName)
	}
	if f.TaskID != "" {
		clauses = append(clauses, "task_id = ?")
		args = append(args, f.TaskID)
	}
	if f.State != "" {
		clauses = append(clauses, "state = ?")
		args = append(args, string(f.State))
	}
	if f.Active {
		clauses = append(clauses, "state IN (?,?,?,?)")
		args = append(args, string(StateStarting), string(StateRunning), string(StateStale), string(StateBlocked))
	}

	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}

	query += " ORDER BY created_at DESC"

	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, ErrDB.Wrap(err, "listing sessions")
	}
	defer func() { _ = rows.Close() }()

	var out []*Session

	for rows.Next() {
		s, sErr := scanSession(rows)
		if sErr != nil {
			return nil, sErr
		}
		out = append(out, s)
	}

	if err := rows.Err(); err != nil {
		return nil, ErrDB.Wrap(err, "listing sessions")
	}

	return out, nil
}

// Delete removes a session row.
func (r *Registry) Delete(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return ErrDB.Wrap(err, "deleting session %s", id)
	}

	return nil
}

// PutHostCheck stores a preflight result.
func (r *Registry) PutHostCheck(ctx context.Context, c *HostCheck) error {
	c.CheckedAt = time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO host_checks (host, ok, os, tmux_version, git_version, runner_version, note, checked_at,
			auth_logged_in, auth_email, auth_subscription)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(host) DO UPDATE SET
			ok = excluded.ok, os = excluded.os, tmux_version = excluded.tmux_version,
			git_version = excluded.git_version, runner_version = excluded.runner_version,
			note = excluded.note, checked_at = excluded.checked_at,
			auth_logged_in = excluded.auth_logged_in, auth_email = excluded.auth_email,
			auth_subscription = excluded.auth_subscription`,
		c.Host, boolInt(c.OK), c.OS, c.TmuxVersion, c.GitVersion, c.RunnerVersion,
		c.Note, ts(c.CheckedAt), nullBool(c.AuthLoggedIn), nullString(c.AuthEmail), nullString(c.AuthSubscription))
	if err != nil {
		return ErrDB.Wrap(err, "storing host check for %s", c.Host)
	}

	return nil
}

// HostCheck reads a cached preflight result. A miss returns (nil, nil).
func (r *Registry) HostCheck(ctx context.Context, host string) (*HostCheck, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT host, ok, os, tmux_version, git_version, runner_version, note, checked_at,
			auth_logged_in, auth_email, auth_subscription
		FROM host_checks WHERE host = ?`, host)

	var (
		c    HostCheck
		ok   int
		at   string
		auth sql.NullInt64
	)

	err := row.Scan(&c.Host, &ok, nullable(&c.OS), nullable(&c.TmuxVersion),
		nullable(&c.GitVersion), nullable(&c.RunnerVersion), nullable(&c.Note), &at,
		&auth, nullable(&c.AuthEmail), nullable(&c.AuthSubscription))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrDB.Wrap(err, "reading host check for %s", host)
	}

	c.OK = ok != 0
	c.CheckedAt, _ = time.Parse(time.RFC3339Nano, at)

	if auth.Valid {
		v := auth.Int64 != 0
		c.AuthLoggedIn = &v
	}

	return &c, nil
}

// --- scanning helpers -------------------------------------------------------

type scanner interface{ Scan(dest ...any) error }

func scanSession(row scanner) (*Session, error) {
	var (
		s                              Session
		remoteURL, promptPreview, note sql.NullString
		lastSeen, ended                sql.NullString
		createdAt, updatedAt, state    string
		panePID                        sql.NullInt64
		exitStatus                     sql.NullInt64
	)

	err := row.Scan(&s.ID, &s.TaskID, &s.Runner, &s.Host, &s.RepoName, &s.RepoDir,
		&s.Worktree, &s.Branch, &s.TmuxSession, &s.LogPath, &remoteURL, &state,
		&panePID, &exitStatus, &promptPreview, &createdAt, &updatedAt,
		&lastSeen, &ended, &note)
	if err != nil {
		return nil, err
	}

	s.RemoteURL = remoteURL.String
	s.State = State(state)
	s.PanePID = int(panePID.Int64)
	s.PromptPreview = promptPreview.String
	s.Note = note.String
	s.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	s.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)

	if exitStatus.Valid {
		v := int(exitStatus.Int64)
		s.ExitStatus = &v
	}
	if lastSeen.Valid {
		if t, pErr := time.Parse(time.RFC3339Nano, lastSeen.String); pErr == nil {
			s.LastSeenAt = &t
		}
	}
	if ended.Valid {
		if t, pErr := time.Parse(time.RFC3339Nano, ended.String); pErr == nil {
			s.EndedAt = &t
		}
	}

	return &s, nil
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

// nullBool stores a tri-state: nil stays NULL, which means "unknown", not false.
func nullBool(b *bool) any {
	if b == nil {
		return nil
	}

	return boolInt(*b)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullable adapts a *string destination to a column that may be NULL.
func nullable(dst *string) any { return &nullStringScanner{dst} }

type nullStringScanner struct{ dst *string }

func (n *nullStringScanner) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*n.dst = ""
	case string:
		*n.dst = v
	case []byte:
		*n.dst = string(v)
	default:
		*n.dst = fmt.Sprint(v)
	}

	return nil
}
