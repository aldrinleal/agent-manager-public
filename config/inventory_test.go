package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestLoadInventoryDefaults(t *testing.T) {
	inv, err := LoadInventory(write(t, `
hosts:
  - name: devbox
    address: dev@devbox
    paths:
      - name: portal
        dir: /home/dev/code/portal
`))
	if err != nil {
		t.Fatal(err)
	}

	h, err := inv.Host("devbox")
	if err != nil {
		t.Fatal(err)
	}

	if h.User != "dev" {
		t.Errorf("user = %q, want dev", h.User)
	}
	// A bare hostname must pick up the default SSH port.
	if h.Addr != "devbox:22" {
		t.Errorf("addr = %q, want devbox:22", h.Addr)
	}
	if h.Runner != "claude-code" {
		t.Errorf("runner = %q, want claude-code", h.Runner)
	}
	if h.Capture != CapturePipePane {
		t.Errorf("capture = %q, want pipe-pane", h.Capture)
	}

	p, err := h.Path("portal")
	if err != nil {
		t.Fatal(err)
	}

	if p.Remote != "origin" || p.Branch != "main" {
		t.Errorf("path defaults = %q/%q, want origin/main", p.Remote, p.Branch)
	}
	if p.Workspace != WorkspaceWorktree {
		t.Errorf("workspace = %q, want worktree", p.Workspace)
	}
	if p.Refresh != StrategyFFOnly {
		t.Errorf("refresh = %q, want ff-only", p.Refresh)
	}
}

// The bastion topology from SPEC.md §4.2: a non-default port, a different
// user, reached only through another inventory host.
func TestLoadInventoryJumpHost(t *testing.T) {
	inv, err := LoadInventory(write(t, `
hosts:
  - name: homelab
    address: dev@localhost:2200
    jump: bastion
    paths:
      - name: notes-svc
        dir: /home/dev/src/notes-svc
  - name: bastion
    address: ubuntu@bastion.example.net
    role: jump
`))
	if err != nil {
		t.Fatal(err)
	}

	h, _ := inv.Host("homelab")
	if h.Addr != "localhost:2200" {
		t.Errorf("addr = %q, want localhost:2200", h.Addr)
	}

	j, _ := inv.Host("bastion")
	if !j.IsJumpOnly() {
		t.Error("bastion should be jump-only")
	}
	// A jump-only host is allowed to declare no paths.
	if len(j.Paths) != 0 {
		t.Errorf("jump host has %d paths, want 0", len(j.Paths))
	}
}

func TestLoadInventoryRejectsCyclicJump(t *testing.T) {
	_, err := LoadInventory(write(t, `
hosts:
  - name: a
    address: u@a
    jump: b
    role: jump
  - name: b
    address: u@b
    jump: a
    role: jump
`))
	if err == nil {
		t.Fatal("expected an error for a cyclic jump chain")
	}

	if !strings.Contains(err.Error(), "cyclic") {
		t.Errorf("error = %v, want it to mention a cycle", err)
	}
}

func TestLoadInventoryRejections(t *testing.T) {
	cases := map[string]string{
		"unknown jump": `
hosts:
  - name: a
    address: u@a
    jump: nope
    paths: [{name: p, dir: /p}]
`,
		"relative dir": `
hosts:
  - name: a
    address: u@a
    paths: [{name: p, dir: relative/path}]
`,
		"no paths and not a jump": `
hosts:
  - name: a
    address: u@a
`,
		"duplicate host": `
hosts:
  - name: a
    address: u@a
    paths: [{name: p, dir: /p}]
  - name: a
    address: u@b
    paths: [{name: p, dir: /p}]
`,
		"bad strategy": `
hosts:
  - name: a
    address: u@a
    paths: [{name: p, dir: /p, refresh: yolo}]
`,
		"bad workspace": `
hosts:
  - name: a
    address: u@a
    paths: [{name: p, dir: /p, workspace: yolo}]
`,
		"empty address": `
hosts:
  - name: a
    address: ""
    paths: [{name: p, dir: /p}]
`,
		"unknown transport": `
hosts:
  - name: a
    transport: telnet
    address: u@a
    paths: [{name: p, dir: /p}]
`,
		"kubectl without pod": `
hosts:
  - name: ws
    transport: kubectl
    namespace: workspaces
    paths: [{name: p, dir: /p}]
`,
		"kubectl with an address": `
hosts:
  - name: ws
    transport: kubectl
    namespace: workspaces
    pod: ws-0
    address: u@ws
    paths: [{name: p, dir: /p}]
`,
		"kubectl as a jump host": `
hosts:
  - name: ws
    transport: kubectl
    namespace: workspaces
    pod: ws-0
    role: jump
`,
		"ssh with a pod": `
hosts:
  - name: a
    address: u@a
    pod: a-0
    paths: [{name: p, dir: /p}]
`,
		"jump through a kubectl host": `
hosts:
  - name: ws
    transport: kubectl
    namespace: workspaces
    pod: ws-0
    paths: [{name: p, dir: /p}]
  - name: a
    address: u@a
    jump: ws
    paths: [{name: p, dir: /p}]
`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadInventory(write(t, body)); err == nil {
				t.Fatalf("expected an error for %s", name)
			}
		})
	}
}

// The allowlist is the security boundary: a directory that is not in
// hosts.yaml must have no way to be named.
func TestPathAllowlist(t *testing.T) {
	inv, err := LoadInventory(write(t, `
hosts:
  - name: a
    address: u@a
    paths: [{name: allowed, dir: /srv/allowed}]
`))
	if err != nil {
		t.Fatal(err)
	}

	h, _ := inv.Host("a")

	if _, err := h.Path("/etc"); err == nil {
		t.Error("a filesystem path should not resolve")
	}
	if _, err := h.Path("not-listed"); err == nil {
		t.Error("an unlisted name should not resolve")
	}
	if _, err := h.Path("allowed"); err != nil {
		t.Errorf("the listed name should resolve: %v", err)
	}
}

func TestObserverRoleNeedsNoPathsButTakesNone(t *testing.T) {
	if _, err := LoadInventory(write(t, "hosts:\n  - name: srv\n    address: u@srv\n    role: observer\n")); err != nil {
		t.Fatalf("an observer needs no paths: %v", err)
	}

	if _, err := LoadInventory(write(t, "hosts:\n  - name: srv\n    address: u@srv\n    role: observer\n    paths: [{name: p, dir: /p}]\n")); err == nil {
		t.Error("an observer with paths is a mistake: nothing is launched on it")
	}
}

func TestUnknownRoleIsRejectedAndAgentHostsStillNeedPaths(t *testing.T) {
	if _, err := LoadInventory(write(t, "hosts:\n  - name: a\n    address: u@a\n    role: boss\n    paths: [{name: p, dir: /p}]\n")); err == nil {
		t.Error("an unknown role must be rejected")
	}

	if _, err := LoadInventory(write(t, "hosts:\n  - name: a\n    address: u@a\n")); err == nil {
		t.Error("a host with no role and no paths is still an error")
	}
}

// Both transports in one inventory: ssh stays the default, a kubectl host
// needs no address, and the inventory-wide identity does not leak onto it.
func TestLoadInventoryMixedTransports(t *testing.T) {
	inv, err := LoadInventory(write(t, `
defaults:
  identity: ~/.ssh/id_agentmgr
hosts:
  - name: laptop-a
    address: dev@laptop-a.local
    paths: [{name: p, dir: /p}]
  - name: ws-go
    transport: KUBECTL
    namespace: workspaces
    pod: ws-go-0
    container: workspace
    paths: [{name: p, dir: /home/agent/p}]
`))
	if err != nil {
		t.Fatal(err)
	}

	ssh, _ := inv.Host("laptop-a")
	if ssh.Transport != TransportSSH || ssh.IsKubectl() || ssh.Identity == "" {
		t.Errorf("laptop-a = %+v, want ssh with the default identity", ssh)
	}

	ws, _ := inv.Host("ws-go")
	if ws.Transport != TransportKubectl || !ws.IsKubectl() {
		t.Errorf("transport = %q, want kubectl (case-insensitive)", ws.Transport)
	}
	if ws.Identity != "" || ws.Addr != "" {
		t.Errorf("a kubectl host has no identity or address, got %q / %q", ws.Identity, ws.Addr)
	}
	if !ws.IsLaunchTarget() {
		t.Error("a kubectl host with paths is a launch target")
	}
}
