package history

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

func ms(daysAgo int) int64 { return now.AddDate(0, 0, -daysAgo).UnixMilli() }

// prompts adds n prompts across `sessions` sessions, last one daysAgo days ago.
func prompts(host, project string, sessions, n, daysAgo int) []Entry {
	var out []Entry

	for i := 0; i < n; i++ {
		out = append(out, Entry{Host: host, SessionID: host + project + string(rune('a'+i%sessions)), TS: ms(daysAgo) - int64(i)*1000, Project: project})
	}

	return out
}

func TestAggregateRanksBySessionsNotPrompts(t *testing.T) {
	// 2067 prompts in ONE marathon session vs 3 sessions of a repo returned to.
	var es []Entry
	es = append(es, prompts("laptop-a", "/s/marathon", 1, 2067, 1)...)
	es = append(es, prompts("laptop-a", "/s/regular", 3, 9, 2)...)

	got := Aggregate(es, Excluder{})

	if got[0].Project != "/s/regular" || got[0].Sessions != 3 || got[1].Sessions != 1 || got[1].Prompts != 2067 {
		t.Fatalf("ranking by prompts let one session win: %+v", got)
	}
}

func TestAggregateExcludesAndCountsSessionsPerHost(t *testing.T) {
	var es []Entry
	es = append(es, prompts("laptop-a", "/s/api-kit", 2, 4, 3)...)
	es = append(es, prompts("laptop-b", "/s/api-kit", 1, 2, 5)...)
	es = append(es, prompts("laptop-a", "/home/dev/src/Course-SCRATCH-x", 3, 6, 1)...)
	es = append(es, prompts("laptop-a", "/home/dev", 1, 3, 1)...)

	got := Aggregate(es, Excluder{Substrings: DefaultExcludes})

	if len(got) != 1 || got[0].Project != "/s/api-kit" {
		t.Fatalf("scratch and the home directory must be excluded: %+v", got)
	}

	if got[0].Sessions != 3 || got[0].Hosts["laptop-a"] != 2 || got[0].Hosts["laptop-b"] != 1 {
		t.Errorf("sessions: %+v", got[0])
	}
}

// The cases below are the real ones from the manual selection of 2026-09-22.
func fixtureResolutions() (map[string]map[string]Resolution, map[string]map[string]bool) {
	git := func(top string, commits bool, def string) Resolution {
		return Resolution{Exists: true, IsGit: true, HasCommits: commits, Top: top, DefaultBranch: def}
	}

	res := map[string]map[string]Resolution{
		"laptop-a": {
			"/s/api-kit":              git("/s/api-kit", true, "master"),
			"/s/vdk":                  git("/s/vdk", true, "master"),
			"/s/flowlab":              git("/s/flowlab", false, ""), // a remote and NO commits
			"/s/data-pipeline/lambda": git("/s/data-pipeline", true, "master"),
			"/s/notes":                {Exists: true}, // a directory, not a repo
			"/s/quiet":                git("/s/quiet", true, "master"),
			"/s/oneoff":               git("/s/oneoff", true, "main"),
			"/s/noorigin":             git("/s/noorigin", true, ""),
		},
		"laptop-b": {
			"/s/api-kit": git("/s/api-kit", true, "master"),
		},
	}

	allow := map[string]map[string]bool{
		"laptop-a": {"/s/vdk": true},
	}

	return res, allow
}

func verdictOf(ps []Proposal, project string) Proposal {
	for _, p := range ps {
		if p.Project == project {
			return p
		}
	}

	return Proposal{}
}

func TestBuildProposalsReproducesTheManualSelection(t *testing.T) {
	var es []Entry
	for _, p := range []struct {
		proj          string
		sess, n, days int
	}{
		{"/s/api-kit", 5, 20, 3}, {"/s/vdk", 6, 12, 2}, {"/s/flowlab", 4, 8, 5},
		{"/s/data-pipeline/lambda", 4, 9, 6}, {"/s/notes", 4, 6, 3}, {"/s/quiet", 4, 8, 200},
		{"/s/oneoff", 1, 2, 4}, {"/s/sh-helper", 6, 12, 156}, {"/s/noorigin", 3, 5, 4},
	} {
		es = append(es, prompts("laptop-a", p.proj, p.sess, p.n, p.days)...)
	}

	res, allow := fixtureResolutions()
	props := BuildProposals(Aggregate(es, Excluder{}), res, allow, now, DefaultProposalOpts)

	cases := map[string]struct{ verdict, reason string }{
		"/s/api-kit":              {VerdictPropose, ""},
		"/s/vdk":                  {VerdictAllowlisted, "allowlist"},
		"/s/flowlab":              {VerdictSkip, "empty repo"}, // the real flowlab case
		"/s/data-pipeline/lambda": {VerdictSkip, "subdirectory of /s/data-pipeline"},
		"/s/notes":                {VerdictSkip, "not a git repository"},
		"/s/quiet":                {VerdictSkip, "dormant 200d"},
		"/s/oneoff":               {VerdictSkip, "only 1 session"},
		"/s/sh-helper":            {VerdictSkip, "not checked out on any host"}, // ranked high, exists nowhere
		"/s/noorigin":             {VerdictPropose, ""},
	}

	for proj, want := range cases {
		got := verdictOf(props, proj)

		if got.Verdict != want.verdict || !strings.Contains(got.Reason, want.reason) {
			t.Errorf("%s: want %s/%q, got %s/%q", proj, want.verdict, want.reason, got.Verdict, got.Reason)
		}
	}
}

func TestProposalPathsAreOnlyForHostsThatHaveItAndDoNotAlreadyAllowIt(t *testing.T) {
	es := prompts("laptop-a", "/s/api-kit", 5, 20, 3)
	res, allow := fixtureResolutions()

	p := verdictOf(BuildProposals(Aggregate(es, Excluder{}), res, allow, now, DefaultProposalOpts), "/s/api-kit")

	if len(p.Paths) != 2 || p.Paths[0].Host != "laptop-a" || p.Paths[1].Host != "laptop-b" || p.Paths[0].Branch != "master" {
		t.Fatalf("%+v", p.Paths)
	}

	// Already allowlisted on laptop-a: only laptop-b remains to be proposed.
	allow["laptop-a"]["/s/api-kit"] = true

	p = verdictOf(BuildProposals(Aggregate(es, Excluder{}), res, allow, now, DefaultProposalOpts), "/s/api-kit")

	if len(p.Paths) != 1 || p.Paths[0].Host != "laptop-b" || len(p.InAllowlist) != 1 {
		t.Errorf("%+v / %v", p.Paths, p.InAllowlist)
	}
}

// A project that exists ONLY on a host we pass no resolution for (an observer,
// such as homelab) is not on any launch host, so it can never be proposed.
func TestAProjectOnlyOnAnObserverIsNeverProposed(t *testing.T) {
	es := prompts("homelab", "/s/chatbot-podman-ecr", 4, 12, 3)

	res := map[string]map[string]Resolution{"laptop-a": {}} // launch hosts only; none has it

	p := verdictOf(BuildProposals(Aggregate(es, Excluder{}), res, nil, now, DefaultProposalOpts), "/s/chatbot-podman-ecr")

	if p.Verdict == VerdictPropose || !strings.Contains(p.Reason, "not checked out") {
		t.Errorf("%+v", p)
	}
}

func TestMissingOriginHeadMakesTheBranchAnExplicitGuess(t *testing.T) {
	es := prompts("laptop-a", "/s/noorigin", 4, 8, 3)
	res, allow := fixtureResolutions()

	p := verdictOf(BuildProposals(Aggregate(es, Excluder{}), res, allow, now, DefaultProposalOpts), "/s/noorigin")

	if len(p.Paths) != 1 || len(p.Paths[0].Warnings) == 0 || !strings.Contains(p.Paths[0].Warnings[0], "GUESS") {
		t.Fatalf("a guessed base branch must be flagged: %+v", p.Paths)
	}

	if y := RenderYAML([]Proposal{p}); !strings.Contains(y, "GUESS") {
		t.Errorf("the warning must reach the YAML a person will paste:\n%s", y)
	}
}

func TestRenderYAMLIsPasteReadyAndGroupedByHost(t *testing.T) {
	es := prompts("laptop-a", "/s/api-kit", 5, 20, 3)
	res, allow := fixtureResolutions()

	y := RenderYAML(BuildProposals(Aggregate(es, Excluder{}), res, allow, now, DefaultProposalOpts))

	for _, want := range []string{`# under host "laptop-a", paths:`, "- name: api-kit", "dir: /s/api-kit", "branch: master", `# under host "laptop-b", paths:`} {
		if !strings.Contains(y, want) {
			t.Errorf("missing %q in:\n%s", want, y)
		}
	}
}

func TestWorklogGroupsByLocalDayProjectAndSession(t *testing.T) {
	loc := time.FixedZone("UTC-5", -5*3600)

	// 23:30 local on the 20th is 04:30 UTC on the 21st: the DAY must be the 20th.
	late := time.Date(2026, 9, 21, 4, 30, 0, 0, time.UTC).UnixMilli()
	next := time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC).UnixMilli()

	es := []Entry{
		{Host: "laptop-a", SessionID: "s1", TS: late, Project: "/s/a", Display: "late night"},
		{Host: "laptop-a", SessionID: "s1", TS: late + 60000, Project: "/s/a", Display: "one more"},
		{Host: "laptop-a", SessionID: "s2", TS: next, Project: "/s/b", Display: "next day"},
		{Host: "laptop-a", SessionID: "s3", TS: next, Project: "/home/dev/MY-SCRATCH", Display: "excluded"},
	}

	days := BuildWorklog(es, Excluder{Substrings: DefaultExcludes}, loc)

	if len(days) != 2 || days[0].Date != "2026-09-21" || days[1].Date != "2026-09-20" {
		t.Fatalf("days newest first, in LOCAL time: %+v", days)
	}

	night := days[1].Projects[0]
	if night.Project != "/s/a" || night.PromptCount != 2 || len(night.Sessions) != 1 || len(night.Sessions[0].Prompts) != 2 {
		t.Errorf("%+v", night)
	}

	if len(days[0].Projects) != 1 {
		t.Errorf("the excluded project leaked: %+v", days[0].Projects)
	}
}

// With prompt storage off the work log degrades to counts; it does not break.
func TestWorklogWithoutPromptTextDegradesToCounts(t *testing.T) {
	es := []Entry{{Host: "h", SessionID: "s", TS: ms(1), Project: "/s/a"}, {Host: "h", SessionID: "s", TS: ms(1) + 1, Project: "/s/a"}}

	days := BuildWorklog(es, Excluder{}, time.UTC)

	if days[0].Projects[0].PromptCount != 2 || len(days[0].Projects[0].Sessions[0].Prompts) != 0 {
		t.Errorf("%+v", days)
	}
}
