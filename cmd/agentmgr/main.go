// Command agentmgr is the CLI: launch and manage remote coding-agent
// sessions, and refresh the checkouts they run in.
//
// docopt + logrus, following ../api-kit and ../cli-kit.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	docopt "github.com/docopt/docopt.go"
	log "github.com/sirupsen/logrus"
	"golang.org/x/term"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/fleet"
	"github.com/aldrinleal/agent-manager-public/gitx"
	"github.com/aldrinleal/agent-manager-public/kubectlx"
	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/sshx"
)

const usage = `agentmgr — remote coding-agent session manager.

Launches ` + "`claude`" + ` inside tmux on a remote host, in a git worktree, and keeps
a registry of what is running where — including the Remote Control URL, which
no vendor API will tell you about.

Usage:
  agentmgr hosts [--json] [--insecure-host-key]
  agentmgr hosts trust <host> [--yes]
  agentmgr check [<host>] [--json] [--insecure-host-key]
  agentmgr up <target> <task-id> (--prompt=<text> | --prompt-file=<path>)
           [--branch=<b>] [--inplace] [--force] [--extra=<args>] [--wait=<s>] [--no-wait]
           [--insecure-host-key]
  agentmgr ls [--host=<h>] [--state=<s>] [--active] [--json] [--insecure-host-key]
  agentmgr url <ref>
  agentmgr log <ref> [--tail=<n>] [--insecure-host-key]
  agentmgr send <ref> <text> [--insecure-host-key]
  agentmgr attach <ref> [--json]
  agentmgr kill <ref> [--remove-worktree] [--insecure-host-key]
  agentmgr forget <ref>
  agentmgr refresh <target> [--strategy=<s>] [--force] [--autostash] [--json] [--insecure-host-key]
  agentmgr inventory render --descriptor=<path> [--check]
  agentmgr history pull [<host>] [--json] [--insecure-host-key]
  agentmgr -h | --help | --version

Global (any command):
  --insecure-host-key   skip SSH host key verification for this run
                        (same as AGENTMGR_SSH_INSECURE_HOST_KEY=true).
                        Logs a warning on every dial.

The API server and web console are a separate binary: agentmgrd (make serve).

Every option is listed explicitly per command rather than via docopt's
[options] shortcut: that shortcut drops any flag which also appears in
another usage line, which silently turned "up --force" into a usage error.

Arguments:
  <target>     host, or host/path (e.g. homelab/notes-svc). A host with exactly
               one path may be named alone.
  <task-id>    short slug identifying the task; becomes the branch
               (agent/<task-id>) and the tmux session name.
  <ref>        a session id, or a task id when it is unambiguous.

Options:
  --prompt=<text>       the task prompt.
  --prompt-file=<path>  read the prompt from a file ("-" for stdin).
  --branch=<b>          base branch to cut the worktree from [default: the path's].
  --inplace             run in the checkout itself instead of a new worktree.
  --force               replace a running session / refresh over local changes.
  --autostash           refresh: let git stash dirty files around a rebase/merge and
                        re-apply them (kept, never dropped, if that conflicts).
  --wait=<s>            seconds to wait for the control URL [default: 120].
  --no-wait             return as soon as tmux is up.
  --extra=<args>        extra arguments for the agent, space separated.
  --host=<h>            filter by host.
  --state=<s>           filter by state (starting|running|stale|stopped|failed).
  --active              only sessions that are starting, running or stale.
  --tail=<n>            lines of output to show [default: 200].
  --strategy=<s>        ff-only | rebase | merge | fetch-only.
  --remove-worktree     also delete the worktree (its work is lost).
  --yes                 do not ask for confirmation.
  --descriptor=<path>   the k8s descriptor whose ConfigMap embeds hosts.yaml.
  --check               exit 1 if the descriptor is out of date; write nothing.
  --insecure-host-key   skip SSH host key verification (see above).
  --json                machine-readable output.
  -h --help             show this.
  --version             show version.
`

// version is stamped by the Makefile via -ldflags.
var version = "dev"

func main() {
	log.SetFormatter(&log.TextFormatter{DisableTimestamp: true})

	opts, err := docopt.ParseArgs(usage, os.Args[1:], "agentmgr "+version)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	// Applied before config.Settings() is first read, so it reaches the pool.
	if insecure, _ := opts.Bool("--insecure-host-key"); insecure {
		_ = os.Setenv("AGENTMGR_SSH_INSECURE_HOST_KEY", "true")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "agentmgr: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, opts docopt.Opts) error {
	// Pure file work: no SSH pool, no registry, no network.
	if isCmd(opts, "inventory") {
		return cmdInventoryRender(opts)
	}

	if isCmd(opts, "history") {
		return cmdHistoryPull(ctx, opts)
	}

	mgr, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = mgr.Close() }()

	switch {
	case isCmd(opts, "hosts") && isCmd(opts, "trust"):
		return cmdTrust(mgr, opts)
	case isCmd(opts, "hosts"):
		return cmdHosts(ctx, mgr, opts)
	case isCmd(opts, "check"):
		return cmdCheck(ctx, mgr, opts)
	case isCmd(opts, "up"):
		return cmdUp(ctx, mgr, opts)
	case isCmd(opts, "ls"):
		return cmdList(ctx, mgr, opts)
	case isCmd(opts, "url"):
		return cmdURL(ctx, mgr, opts)
	case isCmd(opts, "log"):
		return cmdLog(ctx, mgr, opts)
	case isCmd(opts, "send"):
		return cmdSend(ctx, mgr, opts)
	case isCmd(opts, "attach"):
		return cmdAttach(ctx, mgr, opts)
	case isCmd(opts, "kill"):
		return cmdKill(ctx, mgr, opts)
	case isCmd(opts, "forget"):
		return cmdForget(ctx, mgr, opts)
	case isCmd(opts, "refresh"):
		return cmdRefresh(ctx, mgr, opts)
	}

	return fmt.Errorf("no command matched; try --help")
}

func isCmd(opts docopt.Opts, name string) bool {
	b, _ := opts.Bool(name)

	return b
}

// open wires up the fleet for a CLI run.
func open() (*fleet.Fleet, error) {
	cfg := config.Settings()

	inv, err := config.LoadInventory(cfg.InventoryPath)
	if err != nil {
		return nil, err
	}

	// Unlike the server, the CLI may ask for a key passphrase — there is a
	// human here to type one.
	pool, err := sshx.NewPool(cfg, inv, promptPassphrase)
	if err != nil {
		return nil, err
	}

	reg, err := registry.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}

	return fleet.New(cfg, inv, fleet.Dialers{
		SSH:             pool,
		Kubectl:         kubectlx.NewDialer(cfg, inv),
		KeyFingerprints: pool.Keys().Fingerprints(),
	}, reg), nil
}

// promptPassphrase reads a passphrase without echoing it. Only used when
// stdin is a terminal; a piped/non-interactive run fails with a message
// pointing at AGENTMGR_SSH_KEY_PASSPHRASE instead of hanging.
func promptPassphrase(description string) (string, error) {
	fd := int(os.Stdin.Fd())

	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("private key %s is encrypted and stdin is not a terminal; "+
			"set AGENTMGR_SSH_KEY_PASSPHRASE", description)
	}

	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", description)

	pass, err := term.ReadPassword(fd)

	fmt.Fprintln(os.Stderr)

	if err != nil {
		return "", err
	}

	return string(pass), nil
}

// --- commands ---------------------------------------------------------------

func cmdHosts(ctx context.Context, mgr *fleet.Fleet, opts docopt.Opts) error {
	hosts, err := mgr.Hosts(ctx)
	if err != nil {
		return err
	}

	if wantJSON(opts) {
		return printJSON(map[string]any{
			"hosts":            hosts,
			"key_fingerprints": mgr.KeyFingerprints(),
		})
	}

	printHosts(hosts)

	fmt.Println()

	for _, fp := range mgr.KeyFingerprints() {
		fmt.Printf("ssh key: %s\n", fp)
	}

	return nil
}

func cmdTrust(mgr *fleet.Fleet, opts docopt.Opts) error {
	hostName, _ := opts.String("<host>")

	if host, err := mgr.Inventory().Host(hostName); err == nil && host.IsKubectl() {
		return fmt.Errorf("%s uses transport: kubectl: there is no host key to trust, access is the RBAC grant on %s", hostName, host.Namespace)
	}

	pool, err := poolOf(mgr)
	if err != nil {
		return err
	}

	scan, err := pool.Scan(hostName)
	if err != nil {
		return err
	}

	fmt.Printf("host:        %s (%s)\n", scan.HostName, scan.Address)
	fmt.Printf("key type:    %s\n", scan.KeyType)
	fmt.Printf("fingerprint: %s\n", scan.Fingerprint)
	fmt.Println()

	if yes, _ := opts.Bool("--yes"); !yes {
		fmt.Print("Add this key to known_hosts? [y/N] ")

		var answer string
		_, _ = fmt.Scanln(&answer)

		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			return fmt.Errorf("not trusted")
		}
	}

	if err := pool.Trust(scan); err != nil {
		return err
	}

	fmt.Printf("trusted %s\n", scan.HostName)

	return nil
}

func cmdCheck(ctx context.Context, mgr *fleet.Fleet, opts docopt.Opts) error {
	hostName, _ := opts.String("<host>")

	var targets []string

	if hostName != "" {
		targets = []string{hostName}
	} else {
		targets = mgr.Inventory().HostNames()
	}

	var (
		results []*registry.HostCheck
		failed  int
	)

	for _, name := range targets {
		check, err := mgr.Check(ctx, name)

		if check == nil {
			check = &registry.HostCheck{Host: name}
		}
		if err != nil && check.Note == "" {
			check.Note = err.Error()
		}
		if !check.OK {
			failed++
		}

		results = append(results, check)
	}

	if wantJSON(opts) {
		return printJSON(map[string]any{"checks": results})
	}

	printChecks(results)

	if failed > 0 {
		return fmt.Errorf("%d of %d host(s) failed preflight", failed, len(results))
	}

	return nil
}

func cmdUp(ctx context.Context, mgr *fleet.Fleet, opts docopt.Opts) error {
	target, _ := opts.String("<target>")
	taskID, _ := opts.String("<task-id>")

	prompt, err := readPrompt(opts)
	if err != nil {
		return err
	}

	req := fleet.LaunchRequest{
		Host:   target,
		TaskID: taskID,
		Prompt: prompt,
	}

	if b, _ := opts.String("--branch"); b != "" && b != "the path's" {
		req.Branch = b
	}
	if inplace, _ := opts.Bool("--inplace"); inplace {
		req.Workspace = config.WorkspaceInplace
	}
	if force, _ := opts.Bool("--force"); force {
		req.Force = true
	}
	if extra, _ := opts.String("--extra"); extra != "" {
		req.ExtraArgs = strings.Fields(extra)
	}

	session, err := mgr.Launch(ctx, req)
	if err != nil {
		if session != nil {
			printSessions([]*registry.Session{session})
		}

		return err
	}

	printSessions([]*registry.Session{session})

	if noWait, _ := opts.Bool("--no-wait"); noWait {
		return nil
	}

	url, err := waitForURL(ctx, mgr, session.ID, waitSeconds(opts))
	if err != nil {
		return err
	}

	fmt.Println()

	if url == "" {
		fmt.Printf("No Remote Control URL yet. The session is running — reach it with:\n")

		if info, aErr := mgr.Attach(ctx, session.ID); aErr == nil {
			for _, step := range info.Steps() {
				fmt.Printf("  %s\n", step)
			}
		}

		return nil
	}

	fmt.Println(url)

	return nil
}

// waitForURL polls the registry while the scrape goroutine works. Printing a
// spinner would fight with anything reading this output, so it stays quiet.
func waitForURL(ctx context.Context, mgr *fleet.Fleet, id string, seconds int) (string, error) {
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)

	fmt.Fprintf(os.Stderr, "waiting up to %ds for the control URL…\n", seconds)

	for time.Now().Before(deadline) {
		session, err := mgr.Get(ctx, id)
		if err != nil {
			return "", err
		}

		if session.RemoteURL != "" {
			return session.RemoteURL, nil
		}

		if session.State == registry.StateStopped || session.State == registry.StateFailed {
			return "", fmt.Errorf("session %s is %s: %s", session.TaskID, session.State, session.Note)
		}

		select {
		case <-ctx.Done():
			return "", nil
		case <-time.After(time.Second):
		}
	}

	return "", nil
}

func waitSeconds(opts docopt.Opts) int {
	n, err := opts.Int("--wait")
	if err != nil || n <= 0 {
		return 120
	}

	return n
}

func readPrompt(opts docopt.Opts) (string, error) {
	if text, _ := opts.String("--prompt"); text != "" {
		return text, nil
	}

	path, _ := opts.String("--prompt-file")

	if path == "-" {
		raw, err := readAll(os.Stdin)
		if err != nil {
			return "", err
		}

		return raw, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading prompt file: %w", err)
	}

	return string(raw), nil
}

func cmdList(ctx context.Context, mgr *fleet.Fleet, opts docopt.Opts) error {
	filter := registry.Filter{}

	if h, _ := opts.String("--host"); h != "" {
		filter.Host = h
	}
	if s, _ := opts.String("--state"); s != "" {
		filter.State = registry.State(s)
	}
	if a, _ := opts.Bool("--active"); a {
		filter.Active = true
	}

	sessions, err := mgr.List(ctx, filter, true)
	if err != nil {
		return err
	}

	if wantJSON(opts) {
		return printJSON(map[string]any{"sessions": sessions, "count": len(sessions)})
	}

	printSessions(sessions)

	return nil
}

func cmdURL(ctx context.Context, mgr *fleet.Fleet, opts docopt.Opts) error {
	ref, _ := opts.String("<ref>")

	session, err := mgr.Get(ctx, ref)
	if err != nil {
		return err
	}

	if session.RemoteURL == "" {
		return fmt.Errorf("session %s has no control URL (state: %s) — try `agentmgr attach %s`",
			session.TaskID, session.State, ref)
	}

	fmt.Println(session.RemoteURL)

	return nil
}

func cmdLog(ctx context.Context, mgr *fleet.Fleet, opts docopt.Opts) error {
	ref, _ := opts.String("<ref>")

	lines, err := opts.Int("--tail")
	if err != nil {
		lines = 200
	}

	out, err := mgr.Log(ctx, ref, lines)
	if err != nil {
		return err
	}

	fmt.Print(out)

	if !strings.HasSuffix(out, "\n") {
		fmt.Println()
	}

	return nil
}

func cmdSend(ctx context.Context, mgr *fleet.Fleet, opts docopt.Opts) error {
	ref, _ := opts.String("<ref>")
	text, _ := opts.String("<text>")

	if err := mgr.Send(ctx, ref, text); err != nil {
		return err
	}

	fmt.Printf("sent to %s\n", ref)

	return nil
}

func cmdAttach(ctx context.Context, mgr *fleet.Fleet, opts docopt.Opts) error {
	ref, _ := opts.String("<ref>")

	info, err := mgr.Attach(ctx, ref)
	if err != nil {
		return err
	}

	if wantJSON(opts) {
		return printJSON(map[string]any{"attach": info})
	}

	fmt.Printf("%s\n\n", strings.Join(info.Steps(), "\n"))
	fmt.Printf("worktree: %s\nlog:      %s\n", info.Worktree, info.LogPath)

	if info.RemoteURL != "" {
		fmt.Printf("url:      %s\n", info.RemoteURL)
	}

	return nil
}

func cmdKill(ctx context.Context, mgr *fleet.Fleet, opts docopt.Opts) error {
	ref, _ := opts.String("<ref>")
	remove, _ := opts.Bool("--remove-worktree")

	session, err := mgr.Kill(ctx, ref, remove)
	if err != nil {
		return err
	}

	fmt.Printf("killed %s on %s", session.TmuxSession, session.Host)

	if remove {
		fmt.Printf(" (worktree %s removed)", session.Worktree)
	} else {
		fmt.Printf(" (worktree %s kept)", session.Worktree)
	}

	fmt.Println()

	return nil
}

func cmdForget(ctx context.Context, mgr *fleet.Fleet, opts docopt.Opts) error {
	ref, _ := opts.String("<ref>")

	if err := mgr.Forget(ctx, ref); err != nil {
		return err
	}

	fmt.Printf("forgot %s (nothing on the host was touched)\n", ref)

	return nil
}

func cmdRefresh(ctx context.Context, mgr *fleet.Fleet, opts docopt.Opts) error {
	target, _ := opts.String("<target>")
	force, _ := opts.Bool("--force")

	strategy := config.Strategy("")
	if s, _ := opts.String("--strategy"); s != "" {
		strategy = config.Strategy(s)
	}

	autostash, _ := opts.Bool("--autostash")
	ropts := gitx.RefreshOpts{Strategy: strategy, Force: force, Autostash: autostash}

	host, path := target, ""
	if i := strings.Index(target, "/"); i >= 0 {
		host, path = target[:i], target[i+1:]
	}

	if path == "" {
		results, err := mgr.RefreshHost(ctx, host, ropts)

		if wantJSON(opts) {
			return printJSON(map[string]any{"results": results})
		}

		printRefresh(results)

		return err
	}

	result, err := mgr.Refresh(ctx, host, path, ropts)

	if wantJSON(opts) {
		return printJSON(map[string]any{"result": result})
	}

	if result != nil {
		printRefresh([]*gitxResult{result})
	}

	return err
}

// cmdInventoryRender copies the local hosts.yaml into the k8s descriptor's
// ConfigMap, so the CLI and the pod cannot drift (CLAUDE.md, "The inventory
// lives in two places"). The inventory is validated first — a broken file is
// never propagated into the cluster's source of truth.
func cmdInventoryRender(opts docopt.Opts) error {
	descPath, _ := opts.String("--descriptor")
	check, _ := opts.Bool("--check")

	invPath := config.Settings().InventoryPath

	if _, err := config.LoadInventory(invPath); err != nil {
		return fmt.Errorf("%s does not validate, refusing to render it: %w", invPath, err)
	}

	inv, err := os.ReadFile(invPath)
	if err != nil {
		return err
	}

	desc, err := os.ReadFile(descPath)
	if err != nil {
		return err
	}

	out, changed, err := config.RenderIntoDescriptor(desc, inv)
	if err != nil {
		return err
	}

	switch {
	case !changed:
		fmt.Printf("%s is up to date with %s\n", descPath, invPath)
		return nil
	case check:
		return fmt.Errorf("%s is OUT OF DATE with %s (run without --check to update it)", descPath, invPath)
	}

	st, err := os.Stat(descPath)
	if err != nil {
		return err
	}

	if err := os.WriteFile(descPath, out, st.Mode().Perm()); err != nil {
		return err
	}

	fmt.Printf("updated %s from %s — review with `git diff`, then apply + rollout restart\n", descPath, invPath)

	return nil
}

// cmdHistoryPull reads ~/.claude/history.jsonl from one host or all of them
// into the LOCAL registry (the deployed service has its own).
func cmdHistoryPull(ctx context.Context, opts docopt.Opts) error {
	mgr, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = mgr.Close() }()

	var pulls []*registry.HistoryPull

	if host, _ := opts.String("<host>"); host != "" {
		p, err := mgr.PullHistory(ctx, host)
		if err != nil {
			return err
		}

		pulls = []*registry.HistoryPull{p}
	} else {
		pulls = mgr.PullHistoryAll(ctx)
	}

	if wantJSON(opts) {
		return printJSON(map[string]any{"pulls": pulls})
	}

	w := newTabs()

	fmt.Fprintln(w, "HOST\tBYTES\tPARSED\tNEW\tNOTE")

	for _, p := range pulls {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\n", p.Host, p.Bytes, p.Parsed, p.Inserted, firstLine(p.Note))
	}

	return w.Flush()
}
