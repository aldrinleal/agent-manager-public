package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	docopt "github.com/docopt/docopt.go"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/fleet"
	"github.com/aldrinleal/agent-manager-public/gitx"
	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/sshx"
)

// gitxResult is an alias so main.go reads cleanly at its call site.
type gitxResult = gitx.RefreshResult

func wantJSON(opts docopt.Opts) bool {
	b, _ := opts.Bool("--json")

	return b
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	return enc.Encode(v)
}

func newTabs() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

func printHosts(hosts []fleet.HostInfo) {
	w := newTabs()

	fmt.Fprintln(w, "HOST\tADDRESS\tJUMP\tRUNNER\tPATHS\tPREFLIGHT")

	for _, h := range hosts {
		role := h.Address
		if h.Transport == config.TransportKubectl {
			role = "kubectl:" + h.Namespace + "/" + h.Pod
		}
		if h.Role != "" {
			role += " (" + h.Role + ")"
		}

		status := "unchecked"
		if h.Check != nil {
			if h.Check.OK {
				status = "ok " + dim(h.Check.CheckedAt)
			} else {
				status = "FAIL " + firstLine(h.Check.Note)
			}
		}

		paths := strings.Join(pathNames(h.Paths), ",")
		if paths == "" {
			paths = "-"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			h.Name, role, dash(h.Jump), h.Runner, paths, status)
	}

	_ = w.Flush()
}

func pathNames(paths []fleet.PathInfo) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, p.Name)
	}

	return out
}

func printChecks(checks []*registry.HostCheck) {
	w := newTabs()

	fmt.Fprintln(w, "HOST\tOK\tOS\tTMUX\tGIT\tRUNNER\tNOTE")

	for _, c := range checks {
		ok := "yes"
		if !c.OK {
			ok = "NO"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Host, ok, dash(c.OS), dash(shortVersion(c.TmuxVersion)),
			dash(shortVersion(c.GitVersion)), dash(shortVersion(c.RunnerVersion)),
			firstLine(c.Note))
	}

	_ = w.Flush()
}

// shortVersion trims the surrounding prose off a --version string.
//
// The shapes that actually turn up are "git version 2.43.0", "tmux 3.5a",
// "2.1.0 (Claude Code)" and — on a tmux prerelease — "tmux next-3.6". So:
// the first field containing a digit, with any leading prose trimmed off it.
func shortVersion(s string) string {
	for _, f := range strings.Fields(s) {
		if i := strings.IndexFunc(f, isDigit); i >= 0 {
			return f[i:]
		}
	}

	return strings.TrimSpace(s)
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func printSessions(sessions []*registry.Session) {
	if len(sessions) == 0 {
		fmt.Println("no sessions")

		return
	}

	w := newTabs()

	fmt.Fprintln(w, "TASK\tRUNNER\tHOST\tREPO\tBRANCH\tSTATE\tAGE\tURL")

	for _, s := range sessions {
		state := string(s.State)
		if s.ExitStatus != nil {
			state += fmt.Sprintf("(%d)", *s.ExitStatus)
		}

		url := s.RemoteURL
		if url == "" {
			url = "-"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.TaskID, s.Runner, s.Host, s.RepoName, s.Branch, state, age(s.CreatedAt), url)
	}

	_ = w.Flush()

	// Notes carry the reason a session failed or stopped — too long for the
	// table, too important to drop.
	for _, s := range sessions {
		if s.Note != "" {
			fmt.Printf("  %s: %s\n", s.TaskID, firstLine(s.Note))
		}
	}
}

func printRefresh(results []*gitx.RefreshResult) {
	w := newTabs()

	fmt.Fprintln(w, "HOST\tPATH\tSTRATEGY\tBEFORE\tAFTER\tCHANGED\tNOTE")

	for _, r := range results {
		if r == nil {
			continue
		}

		changed := "no"
		if r.Changed {
			changed = "yes"
		}
		if r.Conflict {
			changed = "CONFLICT"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Host, r.Path, r.Strategy, short(r.Before), short(r.After), changed, firstLine(r.Message))
	}

	_ = w.Flush()

	for _, r := range results {
		if r == nil {
			continue
		}

		if r.DiffStat != "" {
			fmt.Printf("\n%s/%s:\n%s\n", r.Host, r.Path, indent(r.DiffStat))
		}

		if r.StashRef != "" {
			fmt.Printf("\n%s/%s: your uncommitted changes are KEPT in stash %s — `git stash pop` once resolved (never dropped for you)\n", r.Host, r.Path, short(r.StashRef))
		}

		if len(r.Conflicts) > 0 {
			fmt.Printf("\n%s/%s conflicts:\n%s\n", r.Host, r.Path, indent(strings.Join(r.Conflicts, "\n")))
		}
	}
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}

	return strings.Join(lines, "\n")
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}

	if sha == "" {
		return "-"
	}

	return sha
}

func dash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)

	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}

	if len(s) > 90 {
		s = s[:90] + "…"
	}

	return s
}

func age(t time.Time) string {
	d := time.Since(t)

	switch {
	case d < 90*time.Second:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < 90*time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func dim(t time.Time) string { return "(" + age(t) + " ago)" }

func readAll(r io.Reader) (string, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}

	return string(raw), nil
}

// poolOf reaches the SSH pool for `hosts trust`, which is the one command
// that needs it directly rather than through fleet.
func poolOf(mgr *fleet.Fleet) (*sshx.Pool, error) {
	pool, ok := mgr.SSH().(*sshx.Pool)
	if !ok || pool == nil {
		return nil, fmt.Errorf("no ssh pool")
	}

	return pool, nil
}
