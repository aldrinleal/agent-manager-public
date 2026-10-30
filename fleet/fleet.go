// Package fleet is the orchestrator: the only thing the CLI and the HTTP API
// talk to. It owns the inventory, the SSH pool, the registry and the
// reconcile loop, and enforces the path allowlist.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/joomcode/errorx"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/transport"
	"github.com/aldrinleal/agent-manager-public/util"
)

var Errors = errorx.NewNamespace("fleet")

var (
	ErrLaunch    = Errors.NewType("launch")
	ErrNotFound  = Errors.NewType("not_found")
	ErrConflict  = Errors.NewType("conflict")
	ErrPreflight = Errors.NewType("preflight")
	ErrBadTarget = Errors.NewType("bad_target")
)

// Dialers are the transports fleet routes hosts to, by each host's
// `transport:` field (SPEC.md §5.4, §5.5). Either may be nil: a host whose
// dialer is missing fails with a clear error instead of being dialed the
// other way. fleet never imports sshx or kubectlx; cmd/ builds these.
type Dialers struct {
	SSH     transport.Dialer
	Kubectl transport.Dialer

	// KeyFingerprints are the SSH key's SHA256 fingerprints, for /hosts. The
	// key itself never reaches fleet.
	KeyFingerprints []string
}

// Fleet ties everything together.
type Fleet struct {
	cfg     *config.Config
	inv     *config.Inventory
	dialers Dialers
	reg     *registry.Registry

	// transports resolves a host name to a transport. It is a field rather
	// than a direct pool call so tests can drive the whole launch path
	// against a fake host (see fleet_test.go).
	transports func(string) (transport.Transport, error)

	// launchSem caps concurrent launches; each one does several round trips
	// plus a git fetch, and a burst of them is how you wedge a small host.
	launchSem chan struct{}

	mu sync.Mutex

	// statusCache holds the last path-status sweep per host (paths.go).
	// Pacing of the auth-code confirmation (auth.go); fields so tests can
	// shorten them.
	authPollEvery time.Duration
	authPollFor   time.Duration

	// resolveCache holds what a host said about a project path (history.go).
	resolveMu    sync.Mutex
	resolveCache map[string]map[string]resolved

	authMu     sync.Mutex
	authTimers map[string]*time.Timer

	statusMu    sync.Mutex
	statusCache map[string]statusEntry
	stopOnce    sync.Once
	stop        chan struct{}
}

// New builds a Fleet. Hosts are dialed lazily, on first use.
func New(cfg *config.Config, inv *config.Inventory, dialers Dialers, reg *registry.Registry) *Fleet {
	max := cfg.MaxConcurrent
	if max < 1 {
		max = 1
	}

	f := &Fleet{
		cfg:           cfg,
		inv:           inv,
		dialers:       dialers,
		reg:           reg,
		launchSem:     make(chan struct{}, max),
		statusCache:   map[string]statusEntry{},
		authPollEvery: 2 * time.Second,
		authPollFor:   30 * time.Second,
		authTimers:    map[string]*time.Timer{},
		resolveCache:  map[string]map[string]resolved{},
		stop:          make(chan struct{}),
	}

	f.transports = f.dial

	return f
}

// Config exposes the loaded settings.
func (f *Fleet) Config() *config.Config { return f.cfg }

// Inventory exposes the parsed hosts.yaml.
func (f *Fleet) Inventory() *config.Inventory { return f.inv }

// Registry exposes the session store.
func (f *Fleet) Registry() *registry.Registry { return f.reg }

// Close stops the reconciler and drops every pooled connection.
func (f *Fleet) Close() error {
	f.stopOnce.Do(func() { close(f.stop) })

	var errs []error

	for _, d := range []transport.Dialer{f.dialers.SSH, f.dialers.Kubectl} {
		if d != nil {
			errs = append(errs, d.Close())
		}
	}

	return errors.Join(errs...)
}

// dial routes a host to the dialer for its transport.
func (f *Fleet) dial(hostName string) (transport.Transport, error) {
	host, err := f.inv.Host(hostName)
	if err != nil {
		return nil, err
	}

	d := f.dialers.SSH
	if host.IsKubectl() {
		d = f.dialers.Kubectl
	}

	if d == nil {
		return nil, ErrPreflight.New("host %q uses transport %s, which is not configured here", host.Name, host.Transport)
	}

	return d.Dial(hostName)
}

// Target is a resolved host + allowed path.
type Target struct {
	Host *config.Host
	Path *config.Path
}

// ResolveTarget parses "host/path" (or a host and path given separately) and
// checks both against the inventory.
//
// This is the allowlist chokepoint: every launch and every refresh comes
// through here, so a directory that is not named in hosts.yaml simply has no
// way to be reached — the API takes a path *name*, never a filesystem path.
func (f *Fleet) ResolveTarget(hostName, pathName string) (*Target, error) {
	if pathName == "" && strings.Contains(hostName, "/") {
		parts := strings.SplitN(hostName, "/", 2)
		hostName, pathName = parts[0], parts[1]
	}

	host, err := f.inv.Host(hostName)
	if err != nil {
		return nil, err
	}

	if !host.IsLaunchTarget() {
		return nil, ErrBadTarget.New("host %q is role: %s — it is not a launch target", hostName, host.Role)
	}

	if pathName == "" {
		if len(host.Paths) == 1 {
			return &Target{Host: host, Path: &host.Paths[0]}, nil
		}

		return nil, ErrBadTarget.New("host %q has %d paths — name one (%s)",
			hostName, len(host.Paths), strings.Join(host.PathNames(), ", "))
	}

	path, err := host.Path(pathName)
	if err != nil {
		return nil, err
	}

	return &Target{Host: host, Path: path}, nil
}

// transportFor returns a transport for a host.
func (f *Fleet) transportFor(hostName string) (transport.Transport, error) {
	return f.transports(hostName)
}

// names computes every derived name for a task, in one place so the CLI, the
// launcher and the reconciler cannot disagree about them.
type names struct {
	TaskID     string
	Session    string
	Branch     string
	Label      string
	Worktree   string
	LogPath    string
	ScriptPath string
	AgentPath  string
	PromptPath string
}

func (f *Fleet) namesFor(t *Target, taskID string) (*names, error) {
	slug := util.Slug(taskID)
	if slug == "" {
		return nil, ErrBadTarget.New("task id %q has no usable characters (want letters, digits, _ or -)", taskID)
	}

	repo := util.Slug(t.Path.Name)

	n := &names{
		TaskID:  slug,
		Session: fmt.Sprintf("agent-%s-%s", repo, slug),
		Branch:  "agent/" + slug,
		Label:   fmt.Sprintf("%s:%s", t.Path.Name, slug),
		LogPath: fmt.Sprintf("%s/%s/%s.log", strings.TrimRight(t.Host.LogRoot, "/"), repo, slug),
	}

	logDir := fmt.Sprintf("%s/%s", strings.TrimRight(t.Host.LogRoot, "/"), repo)

	n.ScriptPath = fmt.Sprintf("%s/%s.launch.sh", logDir, slug)
	n.AgentPath = fmt.Sprintf("%s/%s.agent.sh", logDir, slug)

	// The prompt lives beside the log and the launch script, NOT in the
	// worktree. Writing it into the checkout leaves an untracked file that
	// makes `git status` dirty forever — which then blocks `agentmgr refresh`
	// on an in-place path, and makes every worktree look like it holds work
	// when it does not. Keeping it under log_root also means the prompt
	// survives `git worktree remove`, so a finished task is still auditable.
	n.PromptPath = fmt.Sprintf("%s/%s.prompt.md", logDir, slug)

	switch t.Path.Workspace {
	case config.WorkspaceInplace:
		n.Worktree = t.Path.Dir
	default:
		n.Worktree = fmt.Sprintf("%s/%s/%s", strings.TrimRight(t.Host.WorktreeRoot, "/"), repo, slug)
	}

	return n, nil
}

// preview truncates a prompt for display in the console and `agentmgr ls`.
func preview(s string) string {
	s = strings.Join(strings.Fields(s), " ")

	if len(s) > 200 {
		return s[:200] + "…"
	}

	return s
}

// ctxWithExecTimeout applies the per-command timeout unless the caller
// already set a deadline.
func (f *Fleet) ctxWithExecTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, f.cfg.ExecTimeout)
}

// SSH exposes the SSH dialer, for `agentmgr hosts trust` — the one operation
// that works below the transport abstraction, because it is about the
// connection itself rather than about running anything. cmd/ type-asserts it
// back to the *sshx.Pool it built.
func (f *Fleet) SSH() transport.Dialer { return f.dialers.SSH }
