package history

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// ProjectStat is what the history says about one project path.
type ProjectStat struct {
	Project string `json:"project"`
	// Sessions counts DISTINCT sessions, and is what projects are ranked by —
	// not prompts. One marathon session can hold two thousand prompts
	// (sh-backend-work did); ranking by prompts would let it outrank a repo
	// returned to every day.
	Sessions  int            `json:"sessions"`
	Prompts   int            `json:"prompts"`
	FirstSeen time.Time      `json:"first_seen"`
	LastSeen  time.Time      `json:"last_seen"`
	Hosts     map[string]int `json:"hosts"` // sessions per host
}

// Aggregate groups entries by project, dropping what ex excludes, ranked by
// sessions, then prompts, then name. Callers pre-filter entries by time.
func Aggregate(entries []Entry, ex Excluder) []ProjectStat {
	type acc struct {
		stat     ProjectStat
		sessions map[string]bool
		perHost  map[string]map[string]bool
	}

	byProject := map[string]*acc{}

	for _, e := range entries {
		if skip, _ := ex.Excluded(e.Project); skip {
			continue
		}

		a := byProject[e.Project]
		if a == nil {
			a = &acc{
				stat:     ProjectStat{Project: e.Project, Hosts: map[string]int{}},
				sessions: map[string]bool{},
				perHost:  map[string]map[string]bool{},
			}
			byProject[e.Project] = a
		}

		t := time.UnixMilli(e.TS).UTC()

		a.stat.Prompts++

		if a.stat.FirstSeen.IsZero() || t.Before(a.stat.FirstSeen) {
			a.stat.FirstSeen = t
		}

		if t.After(a.stat.LastSeen) {
			a.stat.LastSeen = t
		}

		// A session id is only meaningful within a host; an entry with none is
		// counted as one session per host rather than dropped.
		key := e.Host + "|" + e.SessionID
		a.sessions[key] = true

		if a.perHost[e.Host] == nil {
			a.perHost[e.Host] = map[string]bool{}
		}

		a.perHost[e.Host][e.SessionID] = true
	}

	out := make([]ProjectStat, 0, len(byProject))

	for _, a := range byProject {
		a.stat.Sessions = len(a.sessions)

		for h, set := range a.perHost {
			a.stat.Hosts[h] = len(set)
		}

		out = append(out, a.stat)
	}

	sort.Slice(out, func(i, j int) bool {
		switch {
		case out[i].Sessions != out[j].Sessions:
			return out[i].Sessions > out[j].Sessions
		case out[i].Prompts != out[j].Prompts:
			return out[i].Prompts > out[j].Prompts
		default:
			return out[i].Project < out[j].Project
		}
	})

	return out
}

// Resolution is what a host says about a project path. It is resolved over the
// transport (see resolve.go), never guessed from the history.
type Resolution struct {
	Exists        bool   `json:"exists"`
	IsGit         bool   `json:"is_git"`
	HasCommits    bool   `json:"has_commits"`
	Top           string `json:"top,omitempty"`            // git toplevel
	DefaultBranch string `json:"default_branch,omitempty"` // from origin/HEAD
}

// Verdicts a Proposal can carry.
const (
	VerdictPropose     = "propose"
	VerdictAllowlisted = "allowlisted"
	VerdictSkip        = "skip"
)

// ProposalOpts tunes BuildProposals.
type ProposalOpts struct {
	MinSessions int // fewer distinct sessions than this is "too little to go on"
	MaxAgeDays  int // last seen longer ago than this is dormant
}

// DefaultProposalOpts is the 2026-09-22 selection: at least 3 sessions, seen
// within 60 days.
var DefaultProposalOpts = ProposalOpts{MinSessions: 3, MaxAgeDays: 60}

// Entry per host in a proposal: a ready-to-paste inventory path.
type ProposedPath struct {
	Host     string   `json:"host"`
	Name     string   `json:"name"`
	Dir      string   `json:"dir"`
	Branch   string   `json:"branch"`
	Warnings []string `json:"warnings,omitempty"`
}

// Proposal is the verdict on one project: add it to the allowlist, or why not.
type Proposal struct {
	ProjectStat
	AgeDays     int            `json:"age_days"`
	Verdict     string         `json:"verdict"`
	Reason      string         `json:"reason,omitempty"`
	Checkouts   []string       `json:"checkouts"`    // launch-target hosts with a usable checkout
	InAllowlist []string       `json:"in_allowlist"` // of those, where it is already allowlisted
	Paths       []ProposedPath `json:"paths,omitempty"`
}

// BuildProposals decides, for each project, whether it belongs in the allowlist.
//
// res maps host -> project path -> Resolution and must cover only hosts that
// are launch targets: a project that exists only on an observer (a server with
// no agents) can never be proposed. allow maps host -> dir -> already allowed.
//
// The checks replicate, in order, what was done by hand on 2026-09-22 — each one
// caught a real case that day, and each has a test:
//
//   - not on any host           (sh-helper, chatbot-podman: ranked high, checked out nowhere)
//   - not a git repository
//   - a subdirectory of a repo  (data-pipeline/lambda-reports)
//   - an EMPTY repo             (flowlab: a remote and no commits cannot host a worktree)
//   - already allowlisted everywhere it exists
//   - too few sessions, or dormant
func BuildProposals(
	stats []ProjectStat,
	res map[string]map[string]Resolution,
	allow map[string]map[string]bool,
	now time.Time,
	o ProposalOpts,
) []Proposal {
	hosts := make([]string, 0, len(res))
	for h := range res {
		hosts = append(hosts, h)
	}

	sort.Strings(hosts)

	out := make([]Proposal, 0, len(stats))

	for _, st := range stats {
		p := Proposal{
			ProjectStat: st,
			AgeDays:     int(now.Sub(st.LastSeen).Hours() / 24),
			Checkouts:   []string{},
			InAllowlist: []string{},
		}

		var (
			anyDir, anyGit bool
			subdirOf       string
			usable         []string
		)

		for _, h := range hosts {
			r := res[h][st.Project]
			if r.Exists {
				anyDir = true
			}

			if r.Exists && r.IsGit {
				anyGit = true

				if r.Top != "" && r.Top != strings.TrimRight(st.Project, "/") && subdirOf == "" {
					subdirOf = r.Top
				}

				if r.HasCommits && (r.Top == "" || r.Top == strings.TrimRight(st.Project, "/")) {
					usable = append(usable, h)
				}
			}
		}

		switch {
		case !anyDir:
			p.Verdict, p.Reason = VerdictSkip, "not checked out on any host"
		case !anyGit:
			p.Verdict, p.Reason = VerdictSkip, "not a git repository"
		case subdirOf != "":
			p.Verdict, p.Reason = VerdictSkip, "subdirectory of "+subdirOf
		case len(usable) == 0:
			p.Verdict, p.Reason = VerdictSkip, "empty repo (no commits: it cannot host a worktree)"
		}

		if p.Verdict == "" {
			p.Checkouts = usable

			var missing []string

			for _, h := range usable {
				if allow[h][st.Project] {
					p.InAllowlist = append(p.InAllowlist, h)
				} else {
					missing = append(missing, h)
				}
			}

			switch {
			case len(missing) == 0:
				p.Verdict, p.Reason = VerdictAllowlisted, "already in the allowlist on every host that has it"
			case st.Sessions < o.MinSessions:
				p.Verdict, p.Reason = VerdictSkip, fmt.Sprintf("only %d session(s); needs %d", st.Sessions, o.MinSessions)
			case p.AgeDays > o.MaxAgeDays:
				p.Verdict, p.Reason = VerdictSkip, fmt.Sprintf("dormant %dd", p.AgeDays)
			default:
				p.Verdict = VerdictPropose

				for _, h := range missing {
					r := res[h][st.Project]
					pp := ProposedPath{Host: h, Name: path.Base(st.Project), Dir: st.Project, Branch: r.DefaultBranch}

					if pp.Branch == "" {
						pp.Branch = "master"
						pp.Warnings = append(pp.Warnings, "no origin/HEAD here, so the base branch is a GUESS: check it")
					}

					p.Paths = append(p.Paths, pp)
				}
			}
		}

		out = append(out, p)
	}

	return out
}

// RenderYAML renders the proposed paths as inventory snippets, grouped by host,
// ready to paste under that host's `paths:` in hosts.yaml. It never edits the
// allowlist: that file is the security boundary, and a person pastes.
func RenderYAML(props []Proposal) string {
	byHost := map[string][]ProposedPath{}

	for _, p := range props {
		if p.Verdict != VerdictPropose {
			continue
		}

		for _, pp := range p.Paths {
			byHost[pp.Host] = append(byHost[pp.Host], pp)
		}
	}

	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}

	sort.Strings(hosts)

	var b strings.Builder

	for _, h := range hosts {
		fmt.Fprintf(&b, "# under host %q, paths:\n", h)

		seen := map[string]bool{}

		for _, pp := range byHost[h] {
			name := pp.Name
			if seen[name] {
				fmt.Fprintf(&b, "    # NOTE: another path on %s is also called %q — rename one\n", h, name)
			}

			seen[name] = true

			for _, w := range pp.Warnings {
				fmt.Fprintf(&b, "    # %s\n", w)
			}

			fmt.Fprintf(&b, "    - name: %s\n      dir: %s\n      branch: %s\n", pp.Name, pp.Dir, pp.Branch)
		}

		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}

// WorkSession is one Claude session's prompts on one day.
type WorkSession struct {
	ID          string    `json:"session_id"`
	Host        string    `json:"host"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	PromptCount int       `json:"prompt_count"`
	// Prompts is empty when prompts are not stored (AGENTMGR_HISTORY_STORE_PROMPTS=false):
	// the log then degrades to counts.
	Prompts []string `json:"prompts,omitempty"`
}

// WorkProject is a day's work in one project.
type WorkProject struct {
	Project     string        `json:"project"`
	PromptCount int           `json:"prompt_count"`
	Sessions    []WorkSession `json:"sessions"`
}

// WorkDay is everything done on one local calendar day.
type WorkDay struct {
	Date     string        `json:"date"`
	Projects []WorkProject `json:"projects"`
}

// BuildWorklog groups entries by local day, then project, then session. The day
// boundary is loc's: a prompt at 23:30 local is that day's, not tomorrow's UTC.
// Days newest first; projects busiest first; sessions and prompts in time order.
func BuildWorklog(entries []Entry, ex Excluder, loc *time.Location) []WorkDay {
	if loc == nil {
		loc = time.UTC
	}

	type skey struct{ day, project, host, sid string }

	sessions := map[skey]*WorkSession{}
	order := []skey{}

	sorted := append([]Entry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].TS < sorted[j].TS })

	for _, e := range sorted {
		if skip, _ := ex.Excluded(e.Project); skip {
			continue
		}

		t := time.UnixMilli(e.TS).In(loc)
		k := skey{t.Format("2006-01-02"), e.Project, e.Host, e.SessionID}

		s := sessions[k]
		if s == nil {
			s = &WorkSession{ID: e.SessionID, Host: e.Host, Start: t}
			sessions[k] = s
			order = append(order, k)
		}

		s.End = t
		s.PromptCount++

		if e.Display != "" {
			s.Prompts = append(s.Prompts, e.Display)
		}
	}

	days := map[string]map[string]*WorkProject{}

	for _, k := range order {
		if days[k.day] == nil {
			days[k.day] = map[string]*WorkProject{}
		}

		wp := days[k.day][k.project]
		if wp == nil {
			wp = &WorkProject{Project: k.project}
			days[k.day][k.project] = wp
		}

		wp.PromptCount += sessions[k].PromptCount
		wp.Sessions = append(wp.Sessions, *sessions[k])
	}

	dates := make([]string, 0, len(days))
	for d := range days {
		dates = append(dates, d)
	}

	sort.Sort(sort.Reverse(sort.StringSlice(dates)))

	out := make([]WorkDay, 0, len(dates))

	for _, d := range dates {
		wd := WorkDay{Date: d}

		for _, wp := range days[d] {
			wd.Projects = append(wd.Projects, *wp)
		}

		sort.Slice(wd.Projects, func(i, j int) bool {
			if wd.Projects[i].PromptCount != wd.Projects[j].PromptCount {
				return wd.Projects[i].PromptCount > wd.Projects[j].PromptCount
			}

			return wd.Projects[i].Project < wd.Projects[j].Project
		})

		out = append(out, wd)
	}

	return out
}
