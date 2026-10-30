package fleet

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/history"
	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/util"
)

// HistoryReadCommand reads a host's Claude Code history. It is ONE fixed string
// on purpose: an observer host's key is restricted to exactly this and
// `uname -s` (scripts/observer/agentmgr-observer.sh). Change it here and that
// wrapper must change with it, deliberately.
const HistoryReadCommand = `cat "$HOME/.claude/history.jsonl"`

// resolveTTL is how long what a host said about a project path is reused.
const resolveTTL = time.Hour

type resolved struct {
	at  time.Time
	res history.Resolution
}

func (f *Fleet) excluder() history.Excluder {
	return history.Excluder{Substrings: history.ParseExcludes(f.cfg.HistoryExclude)}
}

// launchHosts are the hosts agents run on, the only ones a project can be
// proposed for. Observers and jump hosts are read from but never proposed.
func (f *Fleet) launchHosts() []*config.Host {
	var out []*config.Host

	for i := range f.inv.Hosts {
		if h := &f.inv.Hosts[i]; h.IsLaunchTarget() {
			out = append(out, h)
		}
	}

	return out
}

// PullHistory reads one host's history.jsonl and stores what is new.
//
// A host that cannot be reached, or has no history file, is not an error: it is
// recorded in history_pulls with a note and the caller moves on. A failure here
// must never stop the other hosts being read. Only an unknown host name is.
func (f *Fleet) PullHistory(ctx context.Context, hostName string) (*registry.HistoryPull, error) {
	if _, err := f.inv.Host(hostName); err != nil {
		return nil, err
	}

	pull := &registry.HistoryPull{Host: hostName}

	record := func(note string) (*registry.HistoryPull, error) {
		pull.Note = note
		if err := f.reg.PutHistoryPull(ctx, pull); err != nil {
			return pull, err
		}

		return pull, nil
	}

	t, err := f.transportFor(hostName)
	if err != nil {
		return record("unreachable: " + err.Error())
	}

	// A big history takes a while to cat over a slow link.
	execCtx, cancel := context.WithTimeout(ctx, 4*f.cfg.ExecTimeout)
	defer cancel()

	res, err := t.Run(execCtx, HistoryReadCommand)
	if err != nil {
		return record("read failed: " + err.Error())
	}

	if !res.OK() {
		msg := res.Err()
		if msg == "" {
			msg = fmt.Sprintf("exit %d", res.ExitCode)
		}

		return record("no history read: " + msg)
	}

	pull.Bytes = len(res.Stdout)

	entries, st, perr := history.Parse(hostName, strings.NewReader(res.Stdout), f.cfg.HistoryStorePrompts)
	if perr != nil {
		return record("parse failed: " + perr.Error())
	}

	pull.Parsed, pull.Bad = st.Entries, st.Bad

	inserted, err := f.reg.InsertHistory(ctx, entries)
	if err != nil {
		return record("storing failed: " + err.Error())
	}

	pull.Inserted = inserted

	note := ""
	if st.Bad > 0 {
		note = fmt.Sprintf("%d line(s) could not be parsed and were skipped", st.Bad)
	}

	return record(note)
}

// PullHistoryAll reads every host the inventory names, jump hosts excepted
// (they have no claude). Hosts are read concurrently: one asleep laptop must not
// make the others wait out its connect timeout.
func (f *Fleet) PullHistoryAll(ctx context.Context) []*registry.HistoryPull {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out []*registry.HistoryPull
	)

	for i := range f.inv.Hosts {
		h := &f.inv.Hosts[i]
		if h.IsJumpOnly() {
			continue
		}

		wg.Add(1)

		go func() {
			defer wg.Done()

			p, err := f.PullHistory(ctx, h.Name)
			if p == nil {
				p = &registry.HistoryPull{Host: h.Name, Note: fmt.Sprint(err)}
			}

			mu.Lock()
			out = append(out, p)
			mu.Unlock()
		}()
	}

	wg.Wait()

	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })

	return out
}

// StartHistoryCollector pulls every host on a ticker, like the reconciler. The
// first pull is delayed a little so startup is not spent on SSH.
func (f *Fleet) StartHistoryCollector(ctx context.Context) {
	if f.cfg.HistoryPullInterval <= 0 {
		log.Info("fleet: history collector disabled (AGENTMGR_HISTORY_PULL_INTERVAL_S=0)")

		return
	}

	go func() {
		pull := func() {
			ps := f.PullHistoryAll(ctx)

			ok, inserted := 0, 0

			for _, p := range ps {
				if p.Parsed > 0 {
					ok++
				}

				inserted += p.Inserted
			}

			log.Infof("fleet: history pull: %d/%d host(s) read, %d new entries", ok, len(ps), inserted)
		}

		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()

		ticker := time.NewTicker(f.cfg.HistoryPullInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-f.stop:
				return
			case <-timer.C:
				pull()
			case <-ticker.C:
				pull()
			}
		}
	}()

	log.Infof("fleet: collecting history every %s", f.cfg.HistoryPullInterval)
}

// HistoryPulls reports the last pull outcome per host.
func (f *Fleet) HistoryPulls(ctx context.Context) ([]registry.HistoryPull, error) {
	return f.reg.HistoryPulls(ctx)
}

// resolveProjects asks every launch host about every project path, one command
// per host, reusing answers for resolveTTL. Hosts are queried concurrently.
func (f *Fleet) resolveProjects(ctx context.Context, projects []string) map[string]map[string]history.Resolution {
	hosts := f.launchHosts()
	out := make(map[string]map[string]history.Resolution, len(hosts))

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	for _, h := range hosts {
		out[h.Name] = map[string]history.Resolution{}

		var need []string

		f.resolveMu.Lock()
		for _, p := range projects {
			if c, ok := f.resolveCache[h.Name][p]; ok && time.Since(c.at) < resolveTTL {
				out[h.Name][p] = c.res
			} else {
				need = append(need, p)
			}
		}
		f.resolveMu.Unlock()

		if len(need) == 0 {
			continue
		}

		wg.Add(1)

		go func() {
			defer wg.Done()

			t, err := f.transportFor(h.Name)
			if err != nil {
				return // unreachable: its answers are simply absent
			}

			execCtx, cancel := context.WithTimeout(ctx, 3*f.cfg.ExecTimeout)
			defer cancel()

			res, err := t.Run(execCtx, history.ResolveCommand(need))
			if err != nil {
				return
			}

			got := history.ParseResolutions(res.Stdout)

			mu.Lock()
			defer mu.Unlock()

			f.resolveMu.Lock()
			defer f.resolveMu.Unlock()

			if f.resolveCache[h.Name] == nil {
				f.resolveCache[h.Name] = map[string]resolved{}
			}

			for p, r := range got {
				out[h.Name][p] = r
				f.resolveCache[h.Name][p] = resolved{at: time.Now(), res: r}
			}
		}()
	}

	wg.Wait()

	return out
}

// normalizeProject attributes work done in one of agent-manager's own worktrees
// to the repository it belongs to.
//
// A session launched here runs in <worktree_root>/<repo>/<task>, so claude
// records THAT path as the project. Left alone, every throwaway task becomes a
// "project" of its own, drowning the ranking in one-session entries and hiding
// the fact that the work was on the repo. The repo is found by name in the same
// host's inventory; a worktree of something no longer in the inventory is left
// as it is.
func normalizeProject(h *config.Host, project string) string {
	root := strings.TrimRight(h.WorktreeRoot, "/")
	if root == "" || !strings.HasPrefix(project, root+"/") {
		return project
	}

	repo, _, _ := strings.Cut(strings.TrimPrefix(project, root+"/"), "/")

	for _, p := range h.Paths {
		if util.Slug(p.Name) == repo {
			return strings.TrimRight(p.Dir, "/")
		}
	}

	return project
}

// normalizeEntries applies normalizeProject using each entry's own host.
func (f *Fleet) normalizeEntries(entries []history.Entry) []history.Entry {
	out := make([]history.Entry, len(entries))

	for i, e := range entries {
		if h, err := f.inv.Host(e.Host); err == nil {
			e.Project = normalizeProject(h, e.Project)
		}

		out[i] = e
	}

	return out
}

// allowlist maps host -> directory -> allowlisted, for launch hosts.
func (f *Fleet) allowlist() map[string]map[string]bool {
	out := map[string]map[string]bool{}

	for _, h := range f.launchHosts() {
		out[h.Name] = map[string]bool{}
		for _, p := range h.Paths {
			out[h.Name][strings.TrimRight(p.Dir, "/")] = true
		}
	}

	return out
}

func (f *Fleet) historyStats(ctx context.Context, days int) ([]history.ProjectStat, error) {
	if days <= 0 {
		days = 90
	}

	since := time.Now().AddDate(0, 0, -days).UnixMilli()

	entries, err := f.reg.HistorySince(ctx, since, "")
	if err != nil {
		return nil, err
	}

	return history.Aggregate(f.normalizeEntries(entries), f.excluder()), nil
}

// ProjectView is a project's usage plus what the launch hosts say about it.
type ProjectView struct {
	history.ProjectStat
	InAllowlist      bool                          `json:"in_allowlist"`
	InAllowlistHosts []string                      `json:"in_allowlist_hosts"`
	Exists           bool                          `json:"exists"`
	IsGit            bool                          `json:"is_git"`
	HasCommits       bool                          `json:"has_commits"`
	DefaultBranch    string                        `json:"default_branch,omitempty"`
	Resolutions      map[string]history.Resolution `json:"resolutions"`
}

// HistoryProjects ranks every project in the window by distinct sessions and
// resolves each against the launch hosts.
func (f *Fleet) HistoryProjects(ctx context.Context, days int) ([]ProjectView, error) {
	stats, err := f.historyStats(ctx, days)
	if err != nil {
		return nil, err
	}

	paths := make([]string, len(stats))
	for i, s := range stats {
		paths[i] = s.Project
	}

	res := f.resolveProjects(ctx, paths)
	allow := f.allowlist()

	out := make([]ProjectView, 0, len(stats))

	for _, s := range stats {
		v := ProjectView{ProjectStat: s, InAllowlistHosts: []string{}, Resolutions: map[string]history.Resolution{}}

		hosts := make([]string, 0, len(res))
		for h := range res {
			hosts = append(hosts, h)
		}

		sort.Strings(hosts)

		for _, h := range hosts {
			r, ok := res[h][s.Project]
			if !ok {
				continue
			}

			v.Resolutions[h] = r

			if r.Exists {
				v.Exists = true
			}

			if r.IsGit {
				v.IsGit = true
			}

			if r.HasCommits {
				v.HasCommits = true
			}

			if v.DefaultBranch == "" {
				v.DefaultBranch = r.DefaultBranch
			}

			if allow[h][strings.TrimRight(s.Project, "/")] {
				v.InAllowlistHosts = append(v.InAllowlistHosts, h)
			}
		}

		v.InAllowlist = len(v.InAllowlistHosts) > 0
		out = append(out, v)
	}

	return out, nil
}

// ProposalsResult is the verdicts plus the YAML a person can paste.
type ProposalsResult struct {
	Proposals []history.Proposal `json:"proposals"`
	YAML      string             `json:"yaml"`
	// Unreachable names launch hosts that did not answer, whose checkouts are
	// therefore unknown — a "not checked out anywhere" verdict may just mean
	// the laptop is asleep.
	Unreachable []string `json:"unreachable,omitempty"`
}

// HistoryProposals decides which worked-on projects belong in the allowlist. It
// never changes the allowlist: a person pastes the YAML and runs
// `agentmgr inventory render` (CLAUDE.md), because that file is the security boundary.
func (f *Fleet) HistoryProposals(ctx context.Context, days int) (*ProposalsResult, error) {
	stats, err := f.historyStats(ctx, days)
	if err != nil {
		return nil, err
	}

	paths := make([]string, len(stats))
	for i, s := range stats {
		paths[i] = s.Project
	}

	res := f.resolveProjects(ctx, paths)

	props := history.BuildProposals(stats, res, f.allowlist(), time.Now(), history.DefaultProposalOpts)

	out := &ProposalsResult{Proposals: props, YAML: history.RenderYAML(props)}

	// A host that answered for none of the projects is, in practice, unreachable.
	if len(paths) > 0 {
		for h, m := range res {
			if len(m) == 0 {
				out.Unreachable = append(out.Unreachable, h)
			}
		}

		sort.Strings(out.Unreachable)
	}

	return out, nil
}

// HistoryWorklog answers "what did I work on": by day, project and session.
func (f *Fleet) HistoryWorklog(ctx context.Context, host string, days int, tz string) ([]history.WorkDay, error) {
	if days <= 0 {
		days = 7
	}

	if tz == "" {
		tz = f.cfg.TZ
	}

	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, ErrBadTarget.New("unknown time zone %q", tz)
	}

	if host != "" {
		if _, herr := f.inv.Host(host); herr != nil {
			return nil, herr
		}
	}

	// Start of the local day `days` ago, so "7 days" is seven whole days.
	n := time.Now().In(loc)
	start := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))

	entries, err := f.reg.HistorySince(ctx, start.UnixMilli(), host)
	if err != nil {
		return nil, err
	}

	return history.BuildWorklog(f.normalizeEntries(entries), f.excluder(), loc), nil
}
