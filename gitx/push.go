package gitx

import (
	"context"
	"regexp"
	"strings"

	"github.com/aldrinleal/agent-manager-public/transport"
)

// PushResult is what a push did, as the remote reported it.
type PushResult struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	// Output is git's own text, which for GitHub includes the line telling you
	// how to open a pull request.
	Output string `json:"output"`
	// PRURL is that pull-request link when the remote offered one.
	PRURL string `json:"pr_url,omitempty"`
}

// prURL matches the "create a pull request" link GitHub-style remotes print.
var prURL = regexp.MustCompile(`https?://\S+/(?:pull/new|merge_requests/new)\S*`)

// Push pushes a local branch to remote under the same name, optionally
// setting it as the upstream.
//
// There is no force option, and there will not be one: no parameter, no
// environment variable, no "+" in the refspec. A push the remote rejects as
// non-fast-forward comes back as an error carrying the remote's message, and
// what to do about it is a human decision. The refspec is spelled out as
// refs/heads/<b>:refs/heads/<b> so a tag that happens to share the branch's
// name can never be pushed instead.
func Push(ctx context.Context, t transport.Transport, dir, remote, branch string, setUpstream bool) (*PushResult, error) {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " \t\n:^~?*[\\") {
		return nil, ErrGit.New("refusing to push %q: not a plain branch name", branch)
	}

	args := []string{"push"}
	if setUpstream {
		args = append(args, "-u")
	}

	ref := "refs/heads/" + branch
	args = append(args, remote, ref+":"+ref)

	res, err := t.Run(ctx, git(dir, args...))
	if err != nil {
		return nil, err
	}

	// git writes its progress, and GitHub's "remote:" lines, to stderr.
	out := strings.TrimSpace(strings.TrimSpace(res.Stderr) + "\n" + strings.TrimSpace(res.Stdout))

	if !res.OK() {
		return nil, ErrGit.New("push of %s to %s failed (exit %d):\n%s", branch, remote, res.ExitCode, out)
	}

	return &PushResult{Remote: remote, Branch: branch, Output: out, PRURL: ExtractPRURL(out)}, nil
}

// ExtractPRURL pulls the pull-request link out of a push's output, or "".
func ExtractPRURL(out string) string {
	return strings.TrimRight(prURL.FindString(out), ".,)")
}
