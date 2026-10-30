package gitx

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/aldrinleal/agent-manager-public/transport"
	"github.com/aldrinleal/agent-manager-public/util"
)

// RepoStatus is everything a human wants to know about a checkout before
// deciding to refresh it, push it, or start an agent in it.
type RepoStatus struct {
	Head     string `json:"head"`
	Branch   string `json:"branch"`             // "" when detached
	Upstream string `json:"upstream,omitempty"` // "" when none is configured
	Ahead    int    `json:"ahead"`
	Behind   int    `json:"behind"`

	// Dirty lists TRACKED files with changes, staged or not, including
	// unmerged ones. Untracked files are only counted, never listed: they are
	// noise in most checkouts and cannot be lost by a fetch or fast-forward.
	//
	// Untracked counts ENTRIES as git status reports them, so an untracked
	// directory is one entry however many files it holds (`? apks/`). That is
	// deliberate: it matches what `git status` shows the human, where
	// `ls-files --others` would say 2 for the same checkout.
	Dirty     []string `json:"dirty"`
	Untracked int      `json:"untracked"`

	// InProgress is "merge", "rebase" or "cherry-pick" when a previous
	// operation stopped and was deliberately left for a human (CLAUDE.md: a
	// failed merge/rebase is never aborted on your behalf). "" otherwise.
	InProgress string `json:"in_progress,omitempty"`
}

// Clean reports no tracked changes and no operation in progress.
func (s RepoStatus) Clean() bool { return len(s.Dirty) == 0 && s.InProgress == "" }

// ParseStatusV2 parses `git status --porcelain=v2 --branch` output.
//
// Pure, so every shape can be tested without a host. Format reference:
// "# branch.oid|head|upstream|ab" headers, then "1 "/"2 "/"u " entries for
// tracked changes (ordinary, renamed/copied, unmerged) and "? " for untracked.
// Ignored files ("! ") are not requested and are skipped if present.
func ParseStatusV2(out string) RepoStatus {
	st := RepoStatus{Dirty: []string{}}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "# branch.oid "):
			if oid := strings.TrimPrefix(line, "# branch.oid "); oid != "(initial)" {
				st.Head = oid
			}
		case strings.HasPrefix(line, "# branch.head "):
			if h := strings.TrimPrefix(line, "# branch.head "); h != "(detached)" {
				st.Branch = h
			}
		case strings.HasPrefix(line, "# branch.upstream "):
			st.Upstream = strings.TrimPrefix(line, "# branch.upstream ")
		case strings.HasPrefix(line, "# branch.ab "):
			// "+<ahead> -<behind>"
			f := strings.Fields(strings.TrimPrefix(line, "# branch.ab "))
			if len(f) == 2 {
				st.Ahead, _ = strconv.Atoi(strings.TrimPrefix(f[0], "+"))
				st.Behind, _ = strconv.Atoi(strings.TrimPrefix(f[1], "-"))
			}
		case strings.HasPrefix(line, "1 "):
			// 1 XY sub mH mI mW hH hI path        (8 spaces before the path)
			if p := fieldsAfter(line, 8); p != "" {
				st.Dirty = append(st.Dirty, p)
			}
		case strings.HasPrefix(line, "2 "):
			// 2 XY sub mH mI mW hH hI Xscore path\torigPath   (9 spaces)
			if p := fieldsAfter(line, 9); p != "" {
				if tab := strings.IndexByte(p, '\t'); tab >= 0 {
					p = p[:tab]
				}

				st.Dirty = append(st.Dirty, p)
			}
		case strings.HasPrefix(line, "u "):
			// u XY sub m1 m2 m3 mW h1 h2 h3 path   (10 spaces)
			if p := fieldsAfter(line, 10); p != "" {
				st.Dirty = append(st.Dirty, p)
			}
		case strings.HasPrefix(line, "? "):
			st.Untracked++
		}
	}

	return st
}

// fieldsAfter returns the remainder of line after n space-separated fields.
// Paths may contain spaces, so the path must be everything left over rather
// than the n-th field.
func fieldsAfter(line string, n int) string {
	rest := line

	for i := 0; i < n; i++ {
		idx := strings.IndexByte(rest, ' ')
		if idx < 0 {
			return ""
		}

		rest = rest[idx+1:]
	}

	return rest
}

// inProgressProbe prints one of merge / rebase / cherry-pick when git has a
// stopped operation in the checkout. --git-path resolves correctly for a
// linked worktree, whose git dir is not <dir>/.git.
const inProgressProbe = `for f in MERGE_HEAD:merge rebase-merge:rebase rebase-apply:rebase CHERRY_PICK_HEAD:cherry-pick; do ` +
	`n=${f%%:*}; k=${f##*:}; if test -e "$(git rev-parse --git-path "$n")"; then echo "$k"; break; fi; done`

// statusCommand is the compound command that reports one checkout: the v2
// status, a marker, then the in-progress probe. Used alone by GetRepoStatus
// and repeated per path by StatusSweepCommand.
func statusCommand(dir string) string {
	return "cd " + util.ShellQuote(dir) + " && " +
		"git status --porcelain=v2 --branch --untracked-files=normal 2>&1; " +
		"echo '" + inProgressMarker + "'; " + inProgressProbe
}

const (
	inProgressMarker = "@@INPROGRESS"
	sweepPathMarker  = "@@PATH "
	sweepErrMarker   = "@@ERR "
	sweepEndMarker   = "@@END"
)

// GetRepoStatus reports one checkout. One round trip, read-only.
func GetRepoStatus(ctx context.Context, t transport.Transport, dir string) (*RepoStatus, error) {
	res, err := t.Run(ctx, statusCommand(dir))
	if err != nil {
		return nil, err
	}

	st, perr := parseStatusBlock(res.Stdout)
	if perr != nil {
		return nil, ErrGit.New("status of %s: %s", dir, perr.Error())
	}

	return st, nil
}

// parseStatusBlock splits a statusCommand's output at the in-progress marker.
func parseStatusBlock(out string) (*RepoStatus, error) {
	statusPart, probePart, ok := strings.Cut(out, inProgressMarker+"\n")
	if !ok {
		// No marker means `cd` or git itself failed before we got that far.
		return nil, fmt.Errorf("%s", firstNonEmptyLine(out))
	}

	// `git status` failing (not a repo) prints "fatal: ..." with no "# branch" header.
	if !strings.Contains(statusPart, "# branch.") {
		return nil, fmt.Errorf("%s", firstNonEmptyLine(statusPart))
	}

	st := ParseStatusV2(statusPart)
	st.InProgress = strings.TrimSpace(probePart)

	return &st, nil
}

func firstNonEmptyLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}

	return "no output"
}

// SweepEntry is one path's result in a StatusSweep.
type SweepEntry struct {
	Name   string
	Status *RepoStatus
	Err    string
}

// StatusSweepCommand builds ONE command that reports every given path,
// separated by markers — one round trip per host, the same trick as the
// reconcile sweep. A path that is missing or not a repo yields an @@ERR line
// for that path alone, never a failure of the whole sweep.
func StatusSweepCommand(paths []SweepPath) string {
	var b strings.Builder

	for _, p := range paths {
		fmt.Fprintf(&b, "echo %s; ", util.ShellQuote(sweepPathMarker+p.Name))
		fmt.Fprintf(&b, "if (cd %s && git rev-parse --git-dir) >/dev/null 2>&1; then ", util.ShellQuote(p.Dir))
		b.WriteString("(" + statusCommand(p.Dir) + "); ")
		fmt.Fprintf(&b, "else echo %s; fi; ", util.ShellQuote(sweepErrMarker+"missing or not a git repository: "+p.Dir))
		fmt.Fprintf(&b, "echo %s; ", util.ShellQuote(sweepEndMarker))
	}

	return b.String()
}

// SweepPath names a checkout in a sweep.
type SweepPath struct{ Name, Dir string }

// ParseStatusSweep splits a StatusSweepCommand's output back into entries,
// in the order the paths appear in the output.
func ParseStatusSweep(out string) []SweepEntry {
	var entries []SweepEntry

	for _, block := range strings.Split(out, sweepEndMarker+"\n") {
		name, body, ok := strings.Cut(block, "\n")
		if !ok || !strings.HasPrefix(name, sweepPathMarker) {
			continue
		}

		e := SweepEntry{Name: strings.TrimPrefix(name, sweepPathMarker)}

		if msg, isErr := strings.CutPrefix(strings.TrimSpace(body), sweepErrMarker); isErr {
			e.Err = msg
		} else if st, err := parseStatusBlock(body); err != nil {
			e.Err = err.Error()
		} else {
			e.Status = st
		}

		entries = append(entries, e)
	}

	return entries
}
