package gitx

import (
	"context"
	"strings"
	"testing"
)

func TestExtractPRURL(t *testing.T) {
	gh := "remote: \nremote: Create a pull request for 'agent/x' on GitHub by visiting:\nremote:      https://github.com/aldrinleal/agent-manager-public/pull/new/agent/x\nremote: \nTo github.com:aldrinleal/agent-manager-public.git\n * [new branch]      agent/x -> agent/x"

	if got := ExtractPRURL(gh); got != "https://github.com/aldrinleal/agent-manager-public/pull/new/agent/x" {
		t.Errorf("got %q", got)
	}

	if got := ExtractPRURL("Everything up-to-date"); got != "" {
		t.Errorf("no link expected, got %q", got)
	}
}

// There must be no way to ask for a force push, and no force-shaped argument
// may ever reach git — checked on the command actually sent.
func TestPushCommandNeverForces(t *testing.T) {
	var sent string

	_, _ = Push(context.Background(), cmdRecorder{&sent}, "/srv/r", "origin", "agent/x", true)

	for _, bad := range []string{"--force", "-f ", "+refs", "--force-with-lease", "--mirror", "--delete"} {
		if strings.Contains(sent, bad) {
			t.Errorf("push command contains %q: %s", bad, sent)
		}
	}

	if !strings.Contains(sent, "'refs/heads/agent/x:refs/heads/agent/x'") || !strings.Contains(sent, "'-u'") {
		t.Errorf("unexpected push command: %s", sent)
	}
}

func TestPushRefusesBranchNamesThatCouldBeAnythingElse(t *testing.T) {
	for _, b := range []string{"", "-f", "--force", "a b", "a:b", "+x", "a^b", "a~1", "a*", "a?"} {
		var sent string

		_, err := Push(context.Background(), cmdRecorder{&sent}, "/r", "origin", b, false)

		// "+x" is not caught by the character list; assert on the outcome we
		// care about: either refused, or the "+" is inside a refs/heads path
		// and therefore a branch NAME, not a force marker.
		if err == nil && strings.HasPrefix(b, "+") {
			if strings.Contains(sent, "'+") {
				t.Errorf("a leading + reached the refspec: %s", sent)
			}

			continue
		}

		if err == nil {
			t.Errorf("%q should have been refused", b)
		}

		if sent != "" {
			t.Errorf("%q: a refused push must not reach the host: %s", b, sent)
		}
	}
}

func TestPushRealGit(t *testing.T) {
	l := newLab(t)
	tr := localTransport{}
	ctx := context.Background()

	l.sh(l.work, "git checkout -q -b agent/task1 && echo hi > new.txt && git add new.txt && git -c user.name=t -c user.email=t@t commit -qm work")

	res, err := Push(ctx, tr, l.work, "origin", "agent/task1", true)
	if err != nil {
		t.Fatal(err)
	}

	if res.Branch != "agent/task1" {
		t.Errorf("%+v", res)
	}

	if got := strings.TrimSpace(l.sh(l.origin, "git rev-parse --verify -q refs/heads/agent/task1")); got == "" {
		t.Error("the branch did not arrive on the remote")
	}

	if up := strings.TrimSpace(l.sh(l.work, "git rev-parse --abbrev-ref @{u}")); up != "origin/agent/task1" {
		t.Errorf("upstream = %q, want origin/agent/task1", up)
	}

	// Someone else advances the branch; ours is now behind. A plain push is
	// rejected, and must stay rejected: that is the no-force guarantee.
	l.sh(l.upstream, "git fetch -q && git checkout -q -b agent/task1 origin/agent/task1 && echo theirs > theirs.txt && git add . && git -c user.name=t -c user.email=t@t commit -qm theirs && git push -q origin agent/task1")
	l.sh(l.work, "echo mine >> new.txt && git -c user.name=t -c user.email=t@t commit -qam mine")

	_, err = Push(ctx, tr, l.work, "origin", "agent/task1", true)
	if err == nil {
		t.Fatal("a non-fast-forward push must be rejected, not forced")
	}

	if !strings.Contains(err.Error(), "rejected") && !strings.Contains(err.Error(), "failed") {
		t.Errorf("error should carry git's refusal: %v", err)
	}

	// And the remote still has THEIR commit.
	if log := l.sh(l.origin, "git log --format=%s refs/heads/agent/task1"); !strings.Contains(log, "theirs") {
		t.Errorf("the remote lost the other side's work: %q", log)
	}
}
