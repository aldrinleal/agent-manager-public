// Package history reads Claude Code's ~/.claude/history.jsonl and answers two
// questions from it: which repositories are worked on but not allowlisted, and
// what was done in a given period.
//
// Everything here is pure — no host, no database — so it can be tested against
// fixtures, and the shapes that bit the manual run of 2026-09-22 (an empty
// repo, a subdirectory of a bigger repo, a host with no checkouts at all) are
// pinned by tests rather than remembered.
package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

// MaxDisplay bounds how much of a prompt is kept. The prompt text is the user's
// own words and the work log wants it, but a prompt can be a pasted log of
// megabytes; the registry is not the place to hoard that.
const MaxDisplay = 2000

// Entry is one prompt, as history.jsonl records it.
type Entry struct {
	Host      string
	SessionID string
	TS        int64 // milliseconds since the epoch, as in the file
	Project   string
	Display   string // "" when prompts are not being stored

	// Fp fingerprints the prompt's content (project + the ORIGINAL text, even
	// when the text is not stored). It is part of the store's key: two entries
	// in one session can share a millisecond — claude records a repeated /exit
	// three times at once — and without it the key (host, session, ts) would
	// silently drop a genuinely different prompt that landed in the same
	// millisecond. Identical entries still collapse, which is wanted.
	Fp string
}

// ParseStats says what a parse saw, so "parsed 0" is distinguishable from "the
// file was empty" and from "every line was bad".
type ParseStats struct {
	Lines   int
	Entries int
	Bad     int
}

// Parse reads history.jsonl content for one host.
//
// It reads line by line without a line-length limit: a record can carry a
// huge pasted payload, and a bufio.Scanner would stop at its token limit and
// silently drop everything after it. It also counts in Go, because `wc -l`
// once reported 0 lines for a 935-line file here.
//
// A line that is not valid JSON, or has no project or timestamp, is counted as
// Bad and skipped — never fatal, since this file is written by another tool.
func Parse(host string, r io.Reader, storePrompts bool) ([]Entry, ParseStats, error) {
	var (
		entries []Entry
		st      ParseStats
		buf     strings.Builder
		chunk   = make([]byte, 64*1024)
	)

	flush := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}

		st.Lines++

		var raw struct {
			Display   string  `json:"display"`
			Timestamp float64 `json:"timestamp"`
			Project   string  `json:"project"`
			SessionID string  `json:"sessionId"`
		}

		if err := json.Unmarshal([]byte(line), &raw); err != nil || raw.Project == "" || raw.Timestamp <= 0 {
			st.Bad++
			return
		}

		e := Entry{Host: host, SessionID: raw.SessionID, TS: int64(raw.Timestamp), Project: raw.Project, Fp: fingerprint(raw.Project, raw.Display)}

		if storePrompts {
			// Redacted BEFORE it is kept: the registry must never become a second
			// copy of a credential someone pasted into a prompt. (The fingerprint
			// above was taken from the original, and is an 8-byte hash.)
			e.Display = clip(Redact(raw.Display), MaxDisplay)
		}

		entries = append(entries, e)
		st.Entries++
	}

	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])

			s := buf.String()
			for {
				i := strings.IndexByte(s, '\n')
				if i < 0 {
					break
				}

				flush(s[:i])
				s = s[i+1:]
			}

			buf.Reset()
			buf.WriteString(s)
		}

		if err == io.EOF {
			break
		}

		if err != nil {
			return entries, st, err
		}
	}

	flush(buf.String()) // a final line with no trailing newline

	return entries, st, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}

	// Do not cut inside a multi-byte rune.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}

	return s[:n] + "…"
}

// fingerprint is a short, stable digest of what makes an entry distinct.
func fingerprint(project, display string) string {
	sum := sha256.Sum256([]byte(project + "\x00" + display))

	return hex.EncodeToString(sum[:8])
}
