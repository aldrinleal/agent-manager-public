package history

import (
	"strings"

	"github.com/aldrinleal/agent-manager-public/util"
)

const resolvePrefix = "@@P|"

// ResolveCommand builds ONE shell command that inspects every project path on a
// host: does it exist, is it a git work tree, where is its toplevel, what is its
// default branch (origin/HEAD), does it have any commit. One round trip per
// host however many projects there are.
//
// It reads local refs only — no `git remote show origin`, which would hit the
// network once per project. A repository with no origin/HEAD reports an empty
// default branch and the proposal says the base is a guess.
//
// Paths containing "|" cannot be told apart in the output and are skipped.
func ResolveCommand(projects []string) string {
	var b strings.Builder

	for _, p := range projects {
		if strings.ContainsAny(p, "|\n") {
			continue
		}

		q := util.ShellQuote(p)

		b.WriteString("d=" + q + "; ")
		b.WriteString(`if [ ! -d "$d" ]; then echo "` + resolvePrefix + `$d|MISSING"; `)
		b.WriteString(`elif ! top=$(git -C "$d" rev-parse --show-toplevel 2>/dev/null); then echo "` + resolvePrefix + `$d|NOTGIT"; `)
		b.WriteString(`else def=$(git -C "$top" symbolic-ref -q --short refs/remotes/origin/HEAD 2>/dev/null | sed 's#^origin/##'); `)
		b.WriteString(`if git -C "$top" rev-parse -q --verify HEAD >/dev/null 2>&1; then c=1; else c=0; fi; `)
		b.WriteString(`echo "` + resolvePrefix + `$d|OK|$top|$def|$c"; fi; `)
	}

	return b.String()
}

// ParseResolutions reads ResolveCommand's output, keyed by project path.
func ParseResolutions(out string) map[string]Resolution {
	res := map[string]Resolution{}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)

		rest, ok := strings.CutPrefix(line, resolvePrefix)
		if !ok {
			continue
		}

		f := strings.Split(rest, "|")
		if len(f) < 2 {
			continue
		}

		r := Resolution{}

		switch f[1] {
		case "MISSING":
		case "NOTGIT":
			r.Exists = true
		case "OK":
			if len(f) < 5 {
				continue
			}

			r = Resolution{Exists: true, IsGit: true, Top: f[2], DefaultBranch: f[3], HasCommits: f[4] == "1"}
		default:
			continue
		}

		res[f[0]] = r
	}

	return res
}
