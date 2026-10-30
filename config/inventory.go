package config

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/joomcode/errorx"
	"gopkg.in/yaml.v3"

	"github.com/aldrinleal/agent-manager-public/util"
)

var Errors = errorx.NewNamespace("config")

var (
	ErrInventory   = Errors.NewType("inventory")
	ErrUnknownHost = Errors.NewType("unknown_host")
	ErrUnknownPath = Errors.NewType("unknown_path")
)

// Workspace selects how a launch gets a working directory.
type Workspace string

const (
	// WorkspaceWorktree gives every task its own git worktree and branch.
	WorkspaceWorktree Workspace = "worktree"
	// WorkspaceInplace runs in the checkout itself — for single-agent boxes,
	// or repos where worktrees break the toolchain (submodules, vendored deps).
	WorkspaceInplace Workspace = "inplace"
)

// Strategy is how `agentmgr refresh` brings a checkout up to date.
type Strategy string

const (
	StrategyFFOnly    Strategy = "ff-only"
	StrategyRebase    Strategy = "rebase"
	StrategyMerge     Strategy = "merge"
	StrategyFetchOnly Strategy = "fetch-only"
)

// Capture selects how a session's output is tee'd to a log file.
type Capture string

const (
	// CapturePipePane uses `tmux pipe-pane`, so the remote requirement stays
	// exactly linux + tmux + git (SPEC.md §7.4).
	CapturePipePane Capture = "pipe-pane"
	// CaptureScript uses script(1) for a pty-accurate stream. Needs util-linux.
	CaptureScript Capture = "script"
)

// Defaults are the inventory-wide fallbacks for every host.
type Defaults struct {
	Identity     string   `yaml:"identity"`
	Runner       string   `yaml:"runner"`
	WorktreeRoot string   `yaml:"worktree_root"`
	LogRoot      string   `yaml:"log_root"`
	Refresh      Strategy `yaml:"refresh"`
	Capture      Capture  `yaml:"capture"`
}

// Path is one git checkout on a host that agent-manager is allowed to touch.
//
// This is an allowlist: fleet refuses any directory not named here, and the
// API takes a path *name*, never a filesystem path.
type Path struct {
	Name       string    `yaml:"name"`
	Dir        string    `yaml:"dir"`
	Remote     string    `yaml:"remote"`
	Branch     string    `yaml:"branch"`
	Refresh    Strategy  `yaml:"refresh"`
	Runner     string    `yaml:"runner"`
	Workspace  Workspace `yaml:"workspace"`
	AllowDirty bool      `yaml:"allow_dirty"`
}

// Transport selects how commands reach a host (SPEC.md §5.4, §5.5).
type Transport string

const (
	// TransportSSH is a physical machine reached over native SSH. The default.
	TransportSSH Transport = "ssh"
	// TransportKubectl is a workspace pod reached through the API server's
	// pods/exec — no sshd, no key, no DNS.
	TransportKubectl Transport = "kubectl"
)

// Host is a connection method plus the paths reachable through it.
//
// For ssh, Address is "user@host" with an optional ":port". Jump names another
// host in the same inventory to tunnel through — that pair covers both `ssh
// devbox` and `ssh -J ubuntu@bastion -p 2200 dev@localhost`.
//
// For kubectl, Namespace and Pod name the workspace pod (a StatefulSet's is
// `<name>-0`), Container picks one when the pod has several, and Address, Jump
// and Identity do not apply.
type Host struct {
	Name         string            `yaml:"name"`
	Transport    Transport         `yaml:"transport"`
	Address      string            `yaml:"address"`
	Namespace    string            `yaml:"namespace"`
	Pod          string            `yaml:"pod"`
	Container    string            `yaml:"container"`
	Jump         string            `yaml:"jump"`
	Identity     string            `yaml:"identity"`
	Role         string            `yaml:"role"` // "" (agent host), "jump" or "observer"
	Env          map[string]string `yaml:"env"`
	WorktreeRoot string            `yaml:"worktree_root"`
	LogRoot      string            `yaml:"log_root"`
	Runner       string            `yaml:"runner"`
	Capture      Capture           `yaml:"capture"`
	Paths        []Path            `yaml:"paths"`

	// filled in by normalize()
	User string `yaml:"-"`
	Addr string `yaml:"-"` // host:port, ready for net.Dial
}

// Host roles. A host with no role is an agent host: agents are launched on it.
const (
	RoleJump     = "jump"     // exists to be tunnelled through
	RoleObserver = "observer" // reachable and read from, never launched on
)

// IsJumpOnly reports whether this host exists purely to be tunnelled through.
func (h *Host) IsJumpOnly() bool { return h.Role == RoleJump }

// IsObserver reports a host that is checked and read (its history, its
// reachability) but never launched on — a server, or a machine without tmux or
// claude. It takes no paths, and its key is meant to be a restricted one.
func (h *Host) IsObserver() bool { return h.Role == RoleObserver }

// IsKubectl reports a workspace pod reached through pods/exec.
func (h *Host) IsKubectl() bool { return h.Transport == TransportKubectl }

// IsLaunchTarget reports whether agents can be launched on this host.
func (h *Host) IsLaunchTarget() bool { return h.Role == "" }

// Path looks up one of the host's allowed paths by name.
func (h *Host) Path(name string) (*Path, error) {
	for i := range h.Paths {
		if h.Paths[i].Name == name {
			return &h.Paths[i], nil
		}
	}

	return nil, ErrUnknownPath.New("host %q has no path named %q (known: %s)",
		h.Name, name, strings.Join(h.PathNames(), ", "))
}

func (h *Host) PathNames() []string {
	out := make([]string, 0, len(h.Paths))
	for _, p := range h.Paths {
		out = append(out, p.Name)
	}
	return out
}

// Inventory is a parsed hosts.yaml.
type Inventory struct {
	Defaults Defaults `yaml:"defaults"`
	Hosts    []Host   `yaml:"hosts"`

	byName map[string]*Host
}

// LoadInventory reads and validates hosts.yaml.
func LoadInventory(path string) (*Inventory, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrInventory.Wrap(err, "reading %s", path)
	}

	var inv Inventory
	if err := yaml.Unmarshal(raw, &inv); err != nil {
		return nil, ErrInventory.Wrap(err, "parsing %s", path)
	}

	if err := inv.normalize(); err != nil {
		return nil, err
	}

	return &inv, nil
}

// Host looks up a host by name.
func (inv *Inventory) Host(name string) (*Host, error) {
	if h, ok := inv.byName[name]; ok {
		return h, nil
	}

	return nil, ErrUnknownHost.New("no host named %q (known: %s)",
		name, strings.Join(inv.HostNames(), ", "))
}

func (inv *Inventory) HostNames() []string {
	out := make([]string, 0, len(inv.Hosts))
	for i := range inv.Hosts {
		out = append(out, inv.Hosts[i].Name)
	}
	return out
}

// normalize applies defaults, splits addresses, and validates the graph.
func (inv *Inventory) normalize() error {
	if len(inv.Hosts) == 0 {
		return ErrInventory.New("inventory has no hosts")
	}

	d := &inv.Defaults
	if d.Runner == "" {
		d.Runner = "claude-code"
	}
	if d.WorktreeRoot == "" {
		d.WorktreeRoot = "/srv/agent-workspaces"
	}
	if d.LogRoot == "" {
		d.LogRoot = "/srv/agent-logs"
	}
	if d.Refresh == "" {
		d.Refresh = StrategyFFOnly
	}
	if d.Capture == "" {
		d.Capture = CapturePipePane
	}

	inv.byName = make(map[string]*Host, len(inv.Hosts))

	for i := range inv.Hosts {
		h := &inv.Hosts[i]

		if h.Name == "" {
			return ErrInventory.New("hosts[%d] has no name", i)
		}
		if _, dup := inv.byName[h.Name]; dup {
			return ErrInventory.New("duplicate host name %q", h.Name)
		}

		if err := h.normalizeTransport(d); err != nil {
			return err
		}

		if h.Runner == "" {
			h.Runner = d.Runner
		}
		if h.WorktreeRoot == "" {
			h.WorktreeRoot = d.WorktreeRoot
		}
		if h.LogRoot == "" {
			h.LogRoot = d.LogRoot
		}
		if h.Capture == "" {
			h.Capture = d.Capture
		}
		if h.Capture != CapturePipePane && h.Capture != CaptureScript {
			return ErrInventory.New("host %q: unknown capture %q (want pipe-pane or script)", h.Name, h.Capture)
		}

		seen := make(map[string]bool, len(h.Paths))
		for j := range h.Paths {
			p := &h.Paths[j]

			if p.Name == "" {
				return ErrInventory.New("host %q paths[%d] has no name", h.Name, j)
			}
			if seen[p.Name] {
				return ErrInventory.New("host %q has duplicate path name %q", h.Name, p.Name)
			}
			seen[p.Name] = true

			if p.Dir == "" {
				return ErrInventory.New("host %q path %q has no dir", h.Name, p.Name)
			}
			if !strings.HasPrefix(p.Dir, "/") {
				return ErrInventory.New("host %q path %q: dir must be absolute, got %q", h.Name, p.Name, p.Dir)
			}

			if p.Remote == "" {
				p.Remote = "origin"
			}
			if p.Branch == "" {
				p.Branch = "main"
			}
			if p.Refresh == "" {
				p.Refresh = d.Refresh
			}
			if p.Runner == "" {
				p.Runner = h.Runner
			}
			if p.Workspace == "" {
				p.Workspace = WorkspaceWorktree
			}
			if p.Workspace != WorkspaceWorktree && p.Workspace != WorkspaceInplace {
				return ErrInventory.New("host %q path %q: unknown workspace %q (want worktree or inplace)",
					h.Name, p.Name, p.Workspace)
			}
			switch p.Refresh {
			case StrategyFFOnly, StrategyRebase, StrategyMerge, StrategyFetchOnly:
			default:
				return ErrInventory.New("host %q path %q: unknown refresh strategy %q", h.Name, p.Name, p.Refresh)
			}
		}

		h.Role = strings.ToLower(strings.TrimSpace(h.Role))

		switch h.Role {
		case "", RoleJump, RoleObserver:
		default:
			return ErrInventory.New("host %q: unknown role %q (want jump, observer, or nothing)", h.Name, h.Role)
		}

		if h.IsLaunchTarget() && len(h.Paths) == 0 {
			return ErrInventory.New("host %q has no paths (add some, or set role: jump or observer)", h.Name)
		}

		if h.IsObserver() && len(h.Paths) > 0 {
			return ErrInventory.New("host %q is an observer: it takes no paths, since nothing is launched on it", h.Name)
		}

		inv.byName[h.Name] = h
	}

	return inv.validateJumps()
}

// normalizeTransport validates the connection fields for the host's transport
// and rejects the other transport's, so a half-converted entry fails loudly
// instead of being dialed the wrong way.
func (h *Host) normalizeTransport(d *Defaults) error {
	h.Transport = Transport(strings.ToLower(strings.TrimSpace(string(h.Transport))))
	if h.Transport == "" {
		h.Transport = TransportSSH
	}

	switch h.Transport {
	case TransportSSH:
		if h.Namespace != "" || h.Pod != "" || h.Container != "" {
			return ErrInventory.New("host %q: namespace, pod and container are for transport: kubectl", h.Name)
		}

		user, addr, err := splitAddress(h.Address)
		if err != nil {
			return ErrInventory.Wrap(err, "host %q", h.Name)
		}
		h.User, h.Addr = user, addr

		if h.Identity == "" {
			h.Identity = d.Identity
		}
		h.Identity = util.ExpandHome(h.Identity)

	case TransportKubectl:
		if h.Address != "" || h.Jump != "" || h.Identity != "" {
			return ErrInventory.New("host %q: address, jump and identity do not apply to transport: kubectl", h.Name)
		}
		if h.Namespace == "" || h.Pod == "" {
			return ErrInventory.New("host %q: transport: kubectl needs namespace and pod", h.Name)
		}
		if strings.EqualFold(strings.TrimSpace(h.Role), RoleJump) {
			return ErrInventory.New("host %q: a kubectl host cannot be a jump host", h.Name)
		}

	default:
		return ErrInventory.New("host %q: unknown transport %q (want ssh or kubectl)", h.Name, h.Transport)
	}

	return nil
}

// validateJumps checks every jump reference resolves and that the jump graph
// is acyclic — sshx.Dial walks it recursively, so a cycle would hang.
func (inv *Inventory) validateJumps() error {
	for i := range inv.Hosts {
		h := &inv.Hosts[i]

		seen := map[string]bool{h.Name: true}
		for cur := h; cur.Jump != ""; {
			next, ok := inv.byName[cur.Jump]
			if !ok {
				return ErrInventory.New("host %q: jump %q is not in the inventory", cur.Name, cur.Jump)
			}
			if next.IsKubectl() {
				return ErrInventory.New("host %q: jump %q is a kubectl host, which cannot tunnel", cur.Name, cur.Jump)
			}
			if seen[next.Name] {
				return ErrInventory.New("host %q: jump chain is cyclic (revisits %q)", h.Name, next.Name)
			}
			seen[next.Name] = true
			cur = next
		}
	}

	return nil
}

// splitAddress parses "user@host", "user@host:port" or "host" into the SSH
// user and a dialable host:port.
func splitAddress(addr string) (user, hostport string, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", fmt.Errorf("address is empty (want user@host[:port])")
	}

	if at := strings.LastIndex(addr, "@"); at >= 0 {
		user, hostport = addr[:at], addr[at+1:]
	} else {
		hostport = addr
	}

	if user == "" {
		if u := os.Getenv("USER"); u != "" {
			user = u
		} else {
			return "", "", fmt.Errorf("address %q has no user and $USER is unset", addr)
		}
	}

	if hostport == "" {
		return "", "", fmt.Errorf("address %q has no host", addr)
	}

	// Add the default port unless one is already there. net.SplitHostPort
	// errors on a bare host, which is exactly how we detect that case.
	if _, _, splitErr := net.SplitHostPort(hostport); splitErr != nil {
		hostport = net.JoinHostPort(hostport, "22")
	}

	return user, hostport, nil
}
