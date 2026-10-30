package registry

import (
	"context"
	"database/sql"
	"time"

	"github.com/aldrinleal/agent-manager-public/history"
)

// HistoryPull is the outcome of the last attempt to read one host's history.
type HistoryPull struct {
	Host     string    `json:"host"`
	PulledAt time.Time `json:"pulled_at"`
	Bytes    int       `json:"bytes"`
	Parsed   int       `json:"parsed"`
	Inserted int       `json:"inserted"`
	Bad      int       `json:"bad"`
	// Note says why a pull did not happen or what was odd about it: an
	// unreachable host, no history file, lines that would not parse.
	Note string `json:"note,omitempty"`
}

// InsertHistory stores entries, skipping any already present, and returns how
// many were new. Idempotent by construction: the key is (host, session, ts, fingerprint), so
// re-reading a whole file each time — which is what the collector does — costs
// nothing but the read.
func (r *Registry) InsertHistory(ctx context.Context, entries []history.Entry) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, ErrDB.Wrap(err, "starting history insert")
	}

	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO history_entries (host, session_id, ts, fp, project, display)
		VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return 0, ErrDB.Wrap(err, "preparing history insert")
	}

	defer func() { _ = stmt.Close() }()

	inserted := 0

	for _, e := range entries {
		res, err := stmt.ExecContext(ctx, e.Host, e.SessionID, e.TS, e.Fp, e.Project, nullString(e.Display))
		if err != nil {
			return 0, ErrDB.Wrap(err, "inserting a history entry for %s", e.Host)
		}

		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, ErrDB.Wrap(err, "committing history")
	}

	return inserted, nil
}

// HistorySince returns entries at or after sinceMs (a millisecond epoch), oldest
// first, optionally narrowed to one host.
func (r *Registry) HistorySince(ctx context.Context, sinceMs int64, host string) ([]history.Entry, error) {
	q := `SELECT host, session_id, ts, project, COALESCE(display,'') FROM history_entries WHERE ts >= ?`
	args := []any{sinceMs}

	if host != "" {
		q += ` AND host = ?`

		args = append(args, host)
	}

	q += ` ORDER BY ts ASC`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, ErrDB.Wrap(err, "reading history")
	}

	defer func() { _ = rows.Close() }()

	var out []history.Entry

	for rows.Next() {
		var e history.Entry
		if err := rows.Scan(&e.Host, &e.SessionID, &e.TS, &e.Project, &e.Display); err != nil {
			return nil, ErrDB.Wrap(err, "reading history")
		}

		out = append(out, e)
	}

	return out, rows.Err()
}

// PutHistoryPull records the outcome of a pull for a host.
func (r *Registry) PutHistoryPull(ctx context.Context, p *HistoryPull) error {
	p.PulledAt = time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO history_pulls (host, pulled_at, bytes, parsed, inserted, bad, note)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(host) DO UPDATE SET pulled_at = excluded.pulled_at, bytes = excluded.bytes,
			parsed = excluded.parsed, inserted = excluded.inserted, bad = excluded.bad, note = excluded.note`,
		p.Host, ts(p.PulledAt), p.Bytes, p.Parsed, p.Inserted, p.Bad, nullString(p.Note))
	if err != nil {
		return ErrDB.Wrap(err, "recording history pull for %s", p.Host)
	}

	return nil
}

// HistoryPulls returns the last pull outcome per host.
func (r *Registry) HistoryPulls(ctx context.Context) ([]HistoryPull, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT host, pulled_at, bytes, parsed, inserted, bad, COALESCE(note,'') FROM history_pulls ORDER BY host`)
	if err != nil {
		return nil, ErrDB.Wrap(err, "reading history pulls")
	}

	defer func() { _ = rows.Close() }()

	var out []HistoryPull

	for rows.Next() {
		var (
			p  HistoryPull
			at string
		)

		if err := rows.Scan(&p.Host, &at, &p.Bytes, &p.Parsed, &p.Inserted, &p.Bad, &p.Note); err != nil {
			return nil, ErrDB.Wrap(err, "reading history pulls")
		}

		p.PulledAt, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, p)
	}

	return out, rows.Err()
}

// HistoryCount is how many entries are stored, for a host or in total.
func (r *Registry) HistoryCount(ctx context.Context, host string) (int, error) {
	var n int

	var err error

	if host == "" {
		err = r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_entries`).Scan(&n)
	} else {
		err = r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_entries WHERE host = ?`, host).Scan(&n)
	}

	if err != nil && err != sql.ErrNoRows {
		return 0, ErrDB.Wrap(err, "counting history")
	}

	return n, nil
}
