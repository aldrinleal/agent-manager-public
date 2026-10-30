package fleet

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/transport"
)

// recordingDialer remembers which hosts it was asked for.
type recordingDialer struct {
	host   *fakeHost
	dialed []string
	closed bool
}

func (d *recordingDialer) Dial(host string) (transport.Transport, error) {
	d.dialed = append(d.dialed, host)
	return d.host, nil
}

func (d *recordingDialer) Close() error { d.closed = true; return nil }

func mixedInventory(t *testing.T) *config.Inventory {
	t.Helper()

	path := filepath.Join(t.TempDir(), "hosts.yaml")
	body := `
hosts:
  - name: laptop-a
    address: dev@laptop-a.local
    paths: [{name: p, dir: /p}]
  - name: ws-go
    transport: kubectl
    namespace: workspaces
    pod: ws-go-0
    paths: [{name: p, dir: /home/agent/p}]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	inv, err := config.LoadInventory(path)
	if err != nil {
		t.Fatal(err)
	}

	return inv
}

// Each host goes to the dialer for its own transport, never the other one.
func TestDialRoutesByTransport(t *testing.T) {
	ssh := &recordingDialer{host: newFakeHost()}
	kube := &recordingDialer{host: newFakeHost()}

	f := New(&config.Config{MaxConcurrent: 1}, mixedInventory(t), Dialers{SSH: ssh, Kubectl: kube}, nil)

	if _, err := f.transports("laptop-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.transports("ws-go"); err != nil {
		t.Fatal(err)
	}

	if len(ssh.dialed) != 1 || ssh.dialed[0] != "laptop-a" {
		t.Errorf("ssh dialed %v, want [laptop-a]", ssh.dialed)
	}
	if len(kube.dialed) != 1 || kube.dialed[0] != "ws-go" {
		t.Errorf("kubectl dialed %v, want [ws-go]", kube.dialed)
	}

	_ = f.Close()

	if !ssh.closed || !kube.closed {
		t.Error("Close must close both dialers")
	}
}

// A kubectl host with no kubectl dialer fails; it is not dialed over SSH.
func TestDialRefusesAMissingTransport(t *testing.T) {
	ssh := &recordingDialer{host: newFakeHost()}

	f := New(&config.Config{MaxConcurrent: 1}, mixedInventory(t), Dialers{SSH: ssh}, nil)
	defer func() { _ = f.Close() }()

	if _, err := f.transports("ws-go"); err == nil {
		t.Fatal("expected an error for a kubectl host without a kubectl dialer")
	}
	if len(ssh.dialed) != 0 {
		t.Errorf("ssh dialed %v for a kubectl host", ssh.dialed)
	}
}

// A pod is attached in one step, with kubectl exec, and there is no ssh form.
func TestAttachOnAKubectlHost(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), "agentmgr.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()

	cfg := &config.Config{ExecTimeout: 5 * time.Second, URLScrapeTimeout: 3 * time.Second, MaxConcurrent: 1}
	f := New(cfg, mixedInventory(t), Dialers{}, reg)
	defer func() { _ = f.Close() }()

	host := newFakeHost()
	f.transports = func(string) (transport.Transport, error) { return host, nil }

	ctx := context.Background()

	session, err := f.Launch(ctx, LaunchRequest{Host: "ws-go", Path: "p", TaskID: "pod", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}

	info, err := f.Attach(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}

	want := "kubectl -n workspaces exec -it ws-go-0 -- tmux attach -t '=agent-p-pod'"
	if info.ExecCommand != want {
		t.Errorf("exec command = %q, want %q", info.ExecCommand, want)
	}
	if info.SSHCommand != "" {
		t.Errorf("a kubectl host has no ssh command, got %q", info.SSHCommand)
	}
	if steps := info.Steps(); len(steps) != 1 || steps[0] != want {
		t.Errorf("steps = %q", steps)
	}
}
