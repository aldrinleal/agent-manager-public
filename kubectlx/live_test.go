package kubectlx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aldrinleal/agent-manager-public/config"
)

// TestLive runs against a real pod. It is skipped unless
// AGENTMGR_LIVE_POD=<namespace>/<pod> is set; the pod needs sh, cat and tmux.
// AGENTMGR_KUBECONFIG picks the cluster as usual. This is the SPEC §5.5 check:
// a command that starts a detached tmux server must still return promptly.
func TestLive(t *testing.T) {
	target := os.Getenv("AGENTMGR_LIVE_POD")
	if target == "" {
		t.Skip("AGENTMGR_LIVE_POD not set")
	}

	ns, pod, ok := strings.Cut(target, "/")
	if !ok {
		t.Fatalf("AGENTMGR_LIVE_POD = %q, want namespace/pod", target)
	}

	path := filepath.Join(t.TempDir(), "hosts.yaml")
	body := "hosts:\n  - name: live\n    transport: kubectl\n    namespace: " + ns + "\n    pod: " + pod +
		"\n    env: {GREETING: \"it's live\"}\n    paths: [{name: p, dir: /tmp}]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	inv, err := config.LoadInventory(path)
	if err != nil {
		t.Fatal(err)
	}

	tr, err := NewDialer(&config.Config{Kubeconfig: os.Getenv("AGENTMGR_KUBECONFIG")}, inv).Dial("live")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	timed := func(name string, f func()) {
		start := time.Now()
		f()
		t.Logf("%-28s %v", name, time.Since(start).Round(time.Millisecond))
	}

	timed("run (env, exit 0)", func() {
		res, err := tr.Run(ctx, `printf %s "$GREETING"`)
		if err != nil || res.Stdout != "it's live" {
			t.Errorf("got %+v, %v", res, err)
		}
	})

	timed("run (exit 3 is a result)", func() {
		res, err := tr.Run(ctx, "echo oops >&2; exit 3")
		if err != nil || res.ExitCode != 3 || res.Err() != "oops" {
			t.Errorf("got %+v, %v", res, err)
		}
	})

	nasty := "a 'quoted' $(whoami) `x`\nline two\n"
	timed("write + read back", func() {
		if err := tr.WriteFile(ctx, "/tmp/agentmgr-live/p.md", 0o600, strings.NewReader(nasty)); err != nil {
			t.Fatal(err)
		}
		res, err := tr.Run(ctx, "cat /tmp/agentmgr-live/p.md")
		if err != nil || res.Stdout != nasty {
			t.Errorf("content changed in transit: %q, %v", res.Stdout, err)
		}
	})

	// The gotcha: if the detached tmux server kept the stream's fds, this
	// would block until the deadline.
	timed("tmux new-session -d", func() {
		res, err := tr.Run(ctx, "tmux kill-session -t =live 2>/dev/null; tmux new-session -d -s live 'sleep 600' && tmux has-session -t =live && echo up")
		if err != nil || res.Out() != "up" {
			t.Errorf("got %+v, %v", res, err)
		}
	})

	timed("session survives the stream", func() {
		res, err := tr.Run(ctx, "tmux has-session -t =live && echo still-up; tmux kill-session -t =live")
		if err != nil || res.Out() != "still-up" {
			t.Errorf("got %+v, %v", res, err)
		}
	})
}
