package fleet

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/runner"
	"github.com/aldrinleal/agent-manager-public/transport"
)

// checkTTL is how long a successful preflight is trusted before it is re-run.
const checkTTL = 30 * time.Minute

// probeSep separates probe outputs inside the single compound command.
const probeSep = "@@AGENTMGR@@"

// HostInfo is one host as reported to the CLI and the console.
type HostInfo struct {
	Name         string              `json:"name"`
	Transport    config.Transport    `json:"transport"`
	Address      string              `json:"address,omitempty"`
	Namespace    string              `json:"namespace,omitempty"`
	Pod          string              `json:"pod,omitempty"`
	Container    string              `json:"container,omitempty"`
	Jump         string              `json:"jump,omitempty"`
	Role         string              `json:"role,omitempty"`
	Runner       string              `json:"runner"`
	WorktreeRoot string              `json:"worktree_root"`
	LogRoot      string              `json:"log_root"`
	Capture      config.Capture      `json:"capture"`
	Paths        []PathInfo          `json:"paths"`
	Check        *registry.HostCheck `json:"check,omitempty"`
}

// PathInfo is one allowed checkout.
type PathInfo struct {
	Name      string           `json:"name"`
	Dir       string           `json:"dir"`
	Remote    string           `json:"remote"`
	Branch    string           `json:"branch"`
	Refresh   config.Strategy  `json:"refresh"`
	Workspace config.Workspace `json:"workspace"`
	Runner    string           `json:"runner"`
}

// Hosts returns the inventory with each host's cached preflight result.
func (f *Fleet) Hosts(ctx context.Context) ([]HostInfo, error) {
	out := make([]HostInfo, 0, len(f.inv.Hosts))

	for i := range f.inv.Hosts {
		h := &f.inv.Hosts[i]

		info := HostInfo{
			Name:         h.Name,
			Transport:    h.Transport,
			Address:      h.Address,
			Namespace:    h.Namespace,
			Pod:          h.Pod,
			Container:    h.Container,
			Jump:         h.Jump,
			Role:         h.Role,
			Runner:       h.Runner,
			WorktreeRoot: h.WorktreeRoot,
			LogRoot:      h.LogRoot,
			Capture:      h.Capture,
		}

		for j := range h.Paths {
			p := &h.Paths[j]
			info.Paths = append(info.Paths, PathInfo{
				Name:      p.Name,
				Dir:       p.Dir,
				Remote:    p.Remote,
				Branch:    p.Branch,
				Refresh:   p.Refresh,
				Workspace: p.Workspace,
				Runner:    p.Runner,
			})
		}

		check, err := f.reg.HostCheck(ctx, h.Name)
		if err != nil {
			return nil, err
		}
		info.Check = check

		out = append(out, info)
	}

	return out, nil
}

// KeyFingerprints returns the SHA256 fingerprints of the loaded keys. This is
// the only key material that ever leaves the process.
func (f *Fleet) KeyFingerprints() []string { return f.dialers.KeyFingerprints }

// Check runs preflight against a host and caches the result.
//
// One connection, one compound command — a host that is up should cost a
// single round trip to verify (SPEC.md §8).
func (f *Fleet) Check(ctx context.Context, hostName string) (*registry.HostCheck, error) {
	host, err := f.inv.Host(hostName)
	if err != nil {
		return nil, err
	}

	ctx, cancel := f.ctxWithExecTimeout(ctx)
	defer cancel()

	result := &registry.HostCheck{Host: hostName}

	t, err := f.transportFor(hostName)
	if err != nil {
		result.Note = err.Error()
		_ = f.reg.PutHostCheck(ctx, result)

		return result, err
	}

	// An observer is reached with a RESTRICTED key that may run only a couple of
	// fixed commands (scripts/observer), so it gets a plain `uname -s` rather
	// than the compound probe command, which that key would refuse.
	if host.IsObserver() {
		return f.checkObserver(ctx, t, result)
	}

	// A jump-only host needs to be reachable; it runs nothing, so tmux/git
	// are not its problem.
	probes := []runner.Probe{
		{Name: "os", Command: "uname -s", Required: true},
	}

	if host.IsLaunchTarget() {
		probes = append(probes,
			runner.Probe{Name: "tmux", Command: "tmux -V", Required: true},
			runner.Probe{Name: "git", Command: "git --version", Required: true},
		)

		r, rErr := runner.Get(host.Runner)
		if rErr != nil {
			result.Note = rErr.Error()
			_ = f.reg.PutHostCheck(ctx, result)

			return result, rErr
		}

		probes = append(probes, r.Probes()...)
	}

	outputs, err := runProbes(ctx, t, probes)
	if err != nil {
		result.Note = err.Error()
		_ = f.reg.PutHostCheck(ctx, result)

		return result, err
	}

	var missing []string

	for _, p := range probes {
		out, ok := outputs[p.Name]

		switch p.Name {
		case "os":
			result.OS = out
		case "tmux":
			result.TmuxVersion = out
		case "git":
			result.GitVersion = out
		case "auth":
			a := runner.ParseAuthStatus(out)
			result.AuthLoggedIn, result.AuthEmail, result.AuthSubscription = a.LoggedIn, a.Email, a.Subscription
		default:
			result.RunnerVersion = out
		}

		if !ok && p.Required {
			missing = append(missing, p.Name)
		}
	}

	// A host whose claude is logged out passes every other check and then hangs
	// a launch at a login prompt until the URL scrape times out. Only a
	// DEFINITE false fails; unknown (an older claude) does not.
	if result.AuthLoggedIn != nil && !*result.AuthLoggedIn {
		missing = append(missing, "a claude login (run: agentmgr/skill auth-login "+hostName+")")
	}

	if result.OS != "" && !strings.EqualFold(result.OS, "Linux") {
		missing = append(missing, fmt.Sprintf("linux (uname -s says %q)", result.OS))
	}

	if len(missing) > 0 {
		result.OK = false
		result.Note = fmt.Sprintf("missing on %s: %s (need: linux, tmux, git%s)",
			hostName, strings.Join(missing, ", "), runnerNote(host))

		_ = f.reg.PutHostCheck(ctx, result)

		return result, ErrPreflight.New("%s", result.Note)
	}

	result.OK = true
	result.Note = ""

	if err := f.reg.PutHostCheck(ctx, result); err != nil {
		return result, err
	}

	return result, nil
}

func runnerNote(h *config.Host) string {
	if !h.IsLaunchTarget() {
		return ""
	}

	return ", and " + h.Runner + " on PATH"
}

// runProbes runs every probe in one command, separated by a marker, so
// preflight is a single round trip rather than one per tool.
func runProbes(ctx context.Context, t transport.Transport, probes []runner.Probe) (map[string]string, error) {
	var b strings.Builder

	for _, p := range probes {
		// Each probe prints its marker only on success, so a missing tool is
		// visible as an absent section rather than as an empty one.
		fmt.Fprintf(&b, "if { %s ; } 2>/dev/null; then echo '%s%s'; fi\n", p.Command, probeSep, p.Name)
	}

	res, err := t.Run(ctx, b.String())
	if err != nil {
		return nil, err
	}

	out := map[string]string{}

	// Output is interleaved: a probe's own output, then its marker line.
	var pending []string

	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimRight(line, "\r")

		if name, ok := strings.CutPrefix(strings.TrimSpace(line), probeSep); ok {
			out[name] = strings.TrimSpace(strings.Join(pending, " "))
			pending = nil

			continue
		}

		if s := strings.TrimSpace(line); s != "" {
			pending = append(pending, s)
		}
	}

	return out, nil
}

// ensureChecked runs preflight unless a recent successful result is cached.
func (f *Fleet) ensureChecked(ctx context.Context, hostName string) error {
	cached, err := f.reg.HostCheck(ctx, hostName)
	if err != nil {
		return err
	}

	if cached != nil && cached.OK && time.Since(cached.CheckedAt) < checkTTL {
		return nil
	}

	log.Debugf("fleet: preflighting %s", hostName)

	_, err = f.Check(ctx, hostName)

	return err
}

// checkObserver is the whole preflight for an observer: is it reachable, and
// is it Linux. tmux, git and claude are not required of it — that is the point
// of being an observer.
func (f *Fleet) checkObserver(ctx context.Context, t transport.Transport, result *registry.HostCheck) (*registry.HostCheck, error) {
	res, err := t.Run(ctx, "uname -s")
	if err != nil {
		result.Note = err.Error()
		_ = f.reg.PutHostCheck(ctx, result)

		return result, err
	}

	result.OS = res.Out()

	if !res.OK() || !strings.EqualFold(result.OS, "Linux") {
		result.Note = fmt.Sprintf("observer %s: `uname -s` said %q (exit %d): %s", result.Host, result.OS, res.ExitCode, res.Err())
		_ = f.reg.PutHostCheck(ctx, result)

		return result, ErrPreflight.New("%s", result.Note)
	}

	result.OK = true

	return result, f.reg.PutHostCheck(ctx, result)
}
