package fleet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/transport"
)

const inventoryYAML = `
defaults:
  worktree_root: /srv/agent-workspaces
  log_root: /srv/agent-logs
hosts:
  - name: homelab
    address: dev@localhost:2200
    jump: bastion
    env:
      PATH: /home/dev/.local/bin:/usr/bin
    paths:
      - name: notes-svc
        dir: /home/dev/src/notes-svc
      - name: cli-kit
        dir: /home/dev/src/cli-kit
        workspace: inplace
  - name: bastion
    address: ubuntu@bastion.example.net
    role: jump
  - name: server
    address: dev@server.local
    role: observer
`

// newTestFleet builds a Fleet wired to a fake host instead of SSH.
func newTestFleet(t *testing.T) (*Fleet, *fakeHost) {
	t.Helper()

	dir := t.TempDir()

	invPath := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(invPath, []byte(inventoryYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	inv, err := config.LoadInventory(invPath)
	if err != nil {
		t.Fatal(err)
	}

	reg, err := registry.Open(filepath.Join(dir, "agentmgr.db"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		ExecTimeout:         5 * time.Second,
		URLScrapeTimeout:    3 * time.Second,
		StaleAfter:          30 * time.Minute,
		MaxConcurrent:       2,
		ReconcileInterval:   0,
		HistoryStorePrompts: true,
		HistoryExclude:      "scratch,/.openclaw,/.claude",
		TZ:                  "UTC",
	}

	host := newFakeHost()

	f := New(cfg, inv, Dialers{}, reg)
	f.transports = func(string) (transport.Transport, error) { return host, nil }

	t.Cleanup(func() {
		_ = f.Close()
		_ = reg.Close()
	})

	return f, host
}

// A prompt full of shell metacharacters must arrive on the host byte for byte,
// and must not appear anywhere in the launch script.
const nastyPrompt = "Fix the rounding bug.\n" +
	"Don't run `rm -rf /` or $(whoami); the file is called \"tax'.go\".\n" +
	"Then report back.\n"

func TestLaunchEndToEnd(t *testing.T) {
	f, host := newTestFleet(t)
	ctx := context.Background()

	host.paneOutput = "\x1b[1mRemote Control\x1b[0m active at\n" +
		"\x1b[4mhttps://claude.ai/code/session_01ABCdef\x1b[0m\n"

	session, err := f.Launch(ctx, LaunchRequest{
		Host:   "homelab",
		Path:   "notes-svc",
		TaskID: "Fix Checkout/Tax",
		Prompt: nastyPrompt,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// Task id is slugged: tmux would read "." and ":" as target separators.
	if session.TaskID != "Fix-Checkout-Tax" {
		t.Errorf("task id = %q, want Fix-Checkout-Tax", session.TaskID)
	}
	if session.TmuxSession != "agent-notes-svc-Fix-Checkout-Tax" {
		t.Errorf("tmux session = %q", session.TmuxSession)
	}
	if session.Branch != "agent/Fix-Checkout-Tax" {
		t.Errorf("branch = %q", session.Branch)
	}
	if session.Worktree != "/srv/agent-workspaces/notes-svc/Fix-Checkout-Tax" {
		t.Errorf("worktree = %q", session.Worktree)
	}
	if session.State != registry.StateStarting {
		t.Errorf("state = %q, want starting", session.State)
	}

	// --- the prompt reached the host intact, and only via the file ----------

	promptPath := "/srv/agent-logs/notes-svc/Fix-Checkout-Tax.prompt.md"

	got, ok := host.file(promptPath)
	if !ok {
		t.Fatalf("prompt file %s was never written", promptPath)
	}
	if got != nastyPrompt {
		t.Errorf("prompt was mangled in transit:\n%q", got)
	}

	scriptPath := "/srv/agent-logs/notes-svc/Fix-Checkout-Tax.launch.sh"

	script, ok := host.file(scriptPath)
	if !ok {
		t.Fatalf("launch script %s was never written", scriptPath)
	}
	if strings.Contains(script, "rm -rf") || strings.Contains(script, "whoami") {
		t.Fatalf("prompt leaked into the launch script:\n%s", script)
	}
	if !strings.Contains(script, "export PATH='/home/dev/.local/bin:/usr/bin'") {
		t.Errorf("host env was not exported into the script:\n%s", script)
	}

	// --- the right commands, in the right order -----------------------------

	if !host.ran("worktree", "add", "-B", "agent/Fix-Checkout-Tax") {
		t.Error("no worktree was created")
	}
	if !host.ran("tmux new-session -d", "'agent-notes-svc-Fix-Checkout-Tax'", scriptPath) {
		t.Error("tmux new-session did not run the launch script")
	}
	if !host.ran("remain-on-exit on") {
		t.Error("remain-on-exit was not set — exit status would be unrecoverable")
	}
	if !host.ran("pipe-pane", "/srv/agent-logs/notes-svc/Fix-Checkout-Tax.log") {
		t.Error("pipe-pane was not wired to the log")
	}

	// --- the URL is scraped asynchronously ----------------------------------

	url := waitForURL(t, f, session.ID)
	if url != "https://claude.ai/code/session_01ABCdef" {
		t.Errorf("scraped url = %q", url)
	}

	after, err := f.Get(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}

	if after.State != registry.StateRunning {
		t.Errorf("state after URL capture = %q, want running", after.State)
	}
}

func waitForURL(t *testing.T, f *Fleet, id string) string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		s, err := f.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}

		if s.RemoteURL != "" {
			return s.RemoteURL
		}

		time.Sleep(50 * time.Millisecond)
	}

	return ""
}

// A wrapped URL is why capture-pane needs -J. Even so, assert the scrape
// tolerates the joined-but-noisy output a real TUI produces.
func TestLaunchScrapesURLFromNoisyOutput(t *testing.T) {
	f, host := newTestFleet(t)

	host.paneOutput = "\x1b]0;claude\x07" +
		"╭──────────────╮\r\n│ Remote Control │\r\n╰──────────────╯\r\n" +
		"  \x1b[36mhttps://claude.ai/code/session_xyz?src=cli\x1b[0m  \r\n"

	session, err := f.Launch(context.Background(), LaunchRequest{
		Host: "homelab", Path: "notes-svc", TaskID: "noisy", Prompt: "go",
	})
	if err != nil {
		t.Fatal(err)
	}

	if url := waitForURL(t, f, session.ID); url != "https://claude.ai/code/session_xyz?src=cli" {
		t.Errorf("url = %q", url)
	}
}

// A session with no URL is still a usable session — the launch must not fail.
func TestLaunchSucceedsWithoutURL(t *testing.T) {
	f, host := newTestFleet(t)

	host.paneOutput = "no url here\n"

	session, err := f.Launch(context.Background(), LaunchRequest{
		Host: "homelab", Path: "notes-svc", TaskID: "nourl", Prompt: "go",
	})
	if err != nil {
		t.Fatalf("launch should succeed without a URL: %v", err)
	}

	if session.State == registry.StateFailed {
		t.Error("a missing URL must not fail the launch")
	}
}

func TestLaunchInplaceUsesTheCheckoutItself(t *testing.T) {
	f, host := newTestFleet(t)

	session, err := f.Launch(context.Background(), LaunchRequest{
		Host: "homelab", Path: "cli-kit", TaskID: "lint", Prompt: "go",
	})
	if err != nil {
		t.Fatal(err)
	}

	if session.Worktree != "/home/dev/src/cli-kit" {
		t.Errorf("inplace worktree = %q, want the checkout", session.Worktree)
	}
	if session.Branch != "main" {
		t.Errorf("inplace branch = %q, want the path's branch", session.Branch)
	}
	if host.ran("worktree", "add") {
		t.Error("inplace must not create a worktree")
	}

	// Nothing may be written into the checkout: an untracked prompt file
	// would make `git status` dirty forever and block a later refresh.
	for path := range host.files {
		if strings.HasPrefix(path, "/home/dev/src/cli-kit/") {
			t.Errorf("in-place run wrote %s into the checkout", path)
		}
	}

	if _, ok := host.file("/srv/agent-logs/cli-kit/lint.prompt.md"); !ok {
		t.Error("prompt should live under log_root, beside the log and launch script")
	}
}

func TestLaunchRejectsDuplicateTask(t *testing.T) {
	f, _ := newTestFleet(t)
	ctx := context.Background()

	req := LaunchRequest{Host: "homelab", Path: "notes-svc", TaskID: "dup", Prompt: "go"}

	first, err := f.Launch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	second, err := f.Launch(ctx, req)
	if err == nil {
		t.Fatal("relaunching an active task should conflict")
	}

	// The conflict hands back the session already running, so the caller can
	// use it instead of retrying blind.
	if second == nil || second.ID != first.ID {
		t.Errorf("conflict should return the existing session, got %+v", second)
	}
}

func TestLaunchRefusesToClobberAForeignTmuxSession(t *testing.T) {
	f, host := newTestFleet(t)

	host.hasSession = true

	_, err := f.Launch(context.Background(), LaunchRequest{
		Host: "homelab", Path: "notes-svc", TaskID: "taken", Prompt: "go",
	})
	if err == nil {
		t.Fatal("a pre-existing tmux session must not be clobbered")
	}

	if host.ran("tmux new-session") {
		t.Error("new-session ran anyway")
	}
	if host.ran("kill-session") {
		t.Error("kill-session ran without --force")
	}
}

func TestLaunchForceReplacesTheSession(t *testing.T) {
	f, host := newTestFleet(t)

	host.hasSession = true

	if _, err := f.Launch(context.Background(), LaunchRequest{
		Host: "homelab", Path: "notes-svc", TaskID: "taken", Prompt: "go", Force: true,
	}); err != nil {
		t.Fatal(err)
	}

	if !host.ran("kill-session") {
		t.Error("--force should kill the existing session first")
	}
	if !host.ran("tmux new-session") {
		t.Error("--force should then relaunch")
	}
}

func TestLaunchRejectsEmptyPrompt(t *testing.T) {
	f, _ := newTestFleet(t)

	if _, err := f.Launch(context.Background(), LaunchRequest{
		Host: "homelab", Path: "notes-svc", TaskID: "t", Prompt: "   \n",
	}); err == nil {
		t.Fatal("an empty prompt should be rejected")
	}
}

// The allowlist: a directory that is not in hosts.yaml has no way to be named.
func TestResolveTargetEnforcesTheAllowlist(t *testing.T) {
	f, _ := newTestFleet(t)

	for _, bad := range []string{
		"homelab//etc",
		"homelab/../../etc",
		"homelab/not-listed",
		"nosuchhost/notes-svc",
	} {
		if _, err := f.ResolveTarget(bad, ""); err == nil {
			t.Errorf("ResolveTarget(%q) should fail", bad)
		}
	}

	if _, err := f.ResolveTarget("homelab", "notes-svc"); err != nil {
		t.Errorf("a listed path should resolve: %v", err)
	}

	// "host/path" in one string is the CLI's shape.
	target, err := f.ResolveTarget("homelab/notes-svc", "")
	if err != nil {
		t.Fatal(err)
	}

	if target.Path.Dir != "/home/dev/src/notes-svc" {
		t.Errorf("dir = %q", target.Path.Dir)
	}
}

// A jump-only host exists to be tunnelled through, not launched on.
func TestResolveTargetRejectsJumpOnlyHost(t *testing.T) {
	f, _ := newTestFleet(t)

	if _, err := f.ResolveTarget("bastion", ""); err == nil {
		t.Fatal("a role: jump host must not be a launch target")
	}
}

// Ambiguity is an error rather than a guess, but a host with one path is not
// ambiguous.
func TestResolveTargetNeedsAPathWhenThereAreSeveral(t *testing.T) {
	f, _ := newTestFleet(t)

	if _, err := f.ResolveTarget("homelab", ""); err == nil {
		t.Fatal("homelab has two paths; naming one should be required")
	}
}

func TestPreflightFailsLoudlyOnAMissingTool(t *testing.T) {
	f, host := newTestFleet(t)

	host.missingTools["tmux"] = true

	check, err := f.Check(context.Background(), "homelab")
	if err == nil {
		t.Fatal("preflight should fail when tmux is missing")
	}

	if check.OK {
		t.Error("check.OK should be false")
	}
	// The message has to name the tool, not just fail.
	if !strings.Contains(check.Note, "tmux") {
		t.Errorf("note should name tmux, got %q", check.Note)
	}
}

func TestPreflightNamesTheRunnerToo(t *testing.T) {
	f, host := newTestFleet(t)

	host.missingTools["claude"] = true

	check, err := f.Check(context.Background(), "homelab")
	if err == nil {
		t.Fatal("preflight should fail when the runner is missing")
	}

	if !strings.Contains(check.Note, "claude") {
		t.Errorf("note should name claude, got %q", check.Note)
	}
}

func TestPreflightPassesAndCaches(t *testing.T) {
	f, _ := newTestFleet(t)
	ctx := context.Background()

	check, err := f.Check(ctx, "homelab")
	if err != nil {
		t.Fatalf("preflight: %v (%+v)", err, check)
	}

	if !check.OK {
		t.Fatalf("check not OK: %+v", check)
	}
	if check.OS != "Linux" || !strings.Contains(check.TmuxVersion, "3.4") {
		t.Errorf("probe output not captured: %+v", check)
	}

	cached, err := f.Registry().HostCheck(ctx, "homelab")
	if err != nil || cached == nil || !cached.OK {
		t.Errorf("result was not cached: %v %+v", err, cached)
	}
}

// A jump-only host needs to be reachable, but runs nothing — tmux and git are
// not its problem.
func TestPreflightSkipsToolsOnJumpOnlyHosts(t *testing.T) {
	f, host := newTestFleet(t)

	host.missingTools["tmux"] = true
	host.missingTools["git"] = true
	host.missingTools["claude"] = true

	check, err := f.Check(context.Background(), "bastion")
	if err != nil {
		t.Fatalf("a jump host should pass with only uname: %v (%+v)", err, check)
	}

	if !check.OK {
		t.Errorf("check not OK: %+v", check)
	}
}

func TestAttachRendersTheJumpChain(t *testing.T) {
	f, _ := newTestFleet(t)
	ctx := context.Background()

	session, err := f.Launch(ctx, LaunchRequest{
		Host: "homelab", Path: "notes-svc", TaskID: "att", Prompt: "go",
	})
	if err != nil {
		t.Fatal(err)
	}

	info, err := f.Attach(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}

	want := "ssh -J ubuntu@bastion.example.net -p 2200 dev@localhost"
	if info.SSHCommand != want {
		t.Errorf("ssh command = %q, want %q", info.SSHCommand, want)
	}

	if info.TmuxCommand != "tmux attach -t '=agent-notes-svc-att'" {
		t.Errorf("tmux command = %q", info.TmuxCommand)
	}
}

// A failure after new-session must not leave a live agent behind recorded as
// "failed" — nothing would be watching it, and the next launch of the same
// task id would just conflict.
func TestPartialLaunchCleansUpItsTmuxSession(t *testing.T) {
	f, host := newTestFleet(t)

	host.failOn = "pipe-pane"

	session, err := f.Launch(context.Background(), LaunchRequest{
		Host: "homelab", Path: "notes-svc", TaskID: "half", Prompt: "go",
	})
	if err == nil {
		t.Fatal("expected the launch to fail")
	}

	if session.State != registry.StateFailed {
		t.Errorf("state = %q, want failed", session.State)
	}
	if !host.ran("tmux new-session") {
		t.Fatal("test precondition: new-session should have run")
	}
	if !host.ran("kill-session", "agent-notes-svc-half") {
		t.Error("the orphaned tmux session was not cleaned up")
	}
}

// A full-screen TUI's pipe-pane log is a stream of redraws, not a transcript.
// Reading a live session must come from capture-pane; the log file is only
// the fallback for a session tmux no longer has.
func TestLogPrefersCapturePaneForLiveSessions(t *testing.T) {
	f, host := newTestFleet(t)
	ctx := context.Background()

	host.paneOutput = "rendered screen contents\n"

	session, err := f.Launch(ctx, LaunchRequest{
		Host: "homelab", Path: "notes-svc", TaskID: "logtest", Prompt: "go",
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := f.Log(ctx, session.ID, 50)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, "rendered screen contents") {
		t.Errorf("log = %q, want the captured pane", out)
	}

	// A live session must not be read by tailing the raw redraw log.
	var tailed bool

	for _, cmd := range host.Commands() {
		if strings.HasPrefix(cmd, "tail -n") {
			tailed = true
		}
	}

	if tailed {
		t.Error("a live session should be read from capture-pane, not by tailing the log")
	}
}

func TestLogFallsBackToTheLogFileWhenTheSessionIsGone(t *testing.T) {
	f, host := newTestFleet(t)
	ctx := context.Background()

	session, err := f.Launch(ctx, LaunchRequest{
		Host: "homelab", Path: "notes-svc", TaskID: "gone", Prompt: "go",
	})
	if err != nil {
		t.Fatal(err)
	}

	session.State = registry.StateStopped
	if err := f.Registry().Update(ctx, session); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Log(ctx, session.ID, 50); err != nil {
		t.Fatal(err)
	}

	var tailed bool

	for _, cmd := range host.Commands() {
		if strings.HasPrefix(cmd, "tail -n") {
			tailed = true
		}
	}

	if !tailed {
		t.Error("a stopped session should fall back to the log file")
	}
}

// The prompt must never be written inside the worktree. An untracked file
// there makes `git status` dirty forever: it blocks `agentmgr refresh` on an
// in-place path, and makes every worktree look like it holds work when it
// does not.
func TestPromptIsWrittenOutsideTheWorktree(t *testing.T) {
	f, host := newTestFleet(t)

	session, err := f.Launch(context.Background(), LaunchRequest{
		Host: "homelab", Path: "notes-svc", TaskID: "outside", Prompt: "go",
	})
	if err != nil {
		t.Fatal(err)
	}

	host.mu.Lock()
	defer host.mu.Unlock()

	for path := range host.files {
		if strings.HasPrefix(path, session.Worktree+"/") {
			t.Errorf("wrote %s inside the worktree", path)
		}
	}

	if _, ok := host.files["/srv/agent-logs/notes-svc/outside.prompt.md"]; !ok {
		t.Error("prompt should be under log_root")
	}
}

// Preflight must reflect BOTH shapes `claude auth status` really prints.
func TestPreflightRecordsALoggedInHost(t *testing.T) {
	f, _ := newTestFleet(t)

	check, err := f.Check(context.Background(), "homelab")
	if err != nil {
		t.Fatal(err)
	}

	if check.AuthLoggedIn == nil || !*check.AuthLoggedIn || check.AuthEmail != "dev@example.com" || check.AuthSubscription != "max" {
		t.Errorf("auth not recorded: %+v", check)
	}

	// The auth probe's JSON must not have leaked into the runner version.
	if strings.Contains(check.RunnerVersion, "loggedIn") {
		t.Errorf("auth output leaked into runner_version: %q", check.RunnerVersion)
	}
}

// A host whose claude is logged out passes every other check and then hangs a
// launch at a login prompt. Preflight must catch it, and say what to do.
func TestPreflightFailsALoggedOutHost(t *testing.T) {
	f, host := newTestFleet(t)
	host.authOut = `{ "loggedIn": false, "authMethod": "none" }`

	check, err := f.Check(context.Background(), "homelab")
	if err == nil || check.OK {
		t.Fatalf("a logged-out host must fail preflight: %+v", check)
	}

	if !strings.Contains(check.Note, "login") {
		t.Errorf("the note must say what is missing: %q", check.Note)
	}

	if check.AuthLoggedIn == nil || *check.AuthLoggedIn {
		t.Errorf("auth should be a definite false: %+v", check.AuthLoggedIn)
	}

	// And a launch there must be refused up front rather than hanging.
	if _, lerr := f.Launch(context.Background(), planReq()); lerr == nil {
		t.Error("launch on a logged-out host must not proceed")
	}
}

// An older claude with no `auth` subcommand prints nothing useful. That is
// unknown, which must not lock the host out.
func TestPreflightTreatsUnparseableAuthAsUnknownNotLoggedOut(t *testing.T) {
	f, host := newTestFleet(t)
	host.authOut = "error: unknown command 'auth'"

	check, err := f.Check(context.Background(), "homelab")
	if err != nil || !check.OK {
		t.Fatalf("unknown auth must not fail preflight: %v %+v", err, check)
	}

	if check.AuthLoggedIn != nil {
		t.Errorf("unknown must stay nil, got %v", *check.AuthLoggedIn)
	}
}

func TestObserverIsCheckedWithAPlainUnameAndNothingElse(t *testing.T) {
	f, host := newTestFleet(t)

	check, err := f.Check(context.Background(), "server")
	if err != nil || !check.OK || check.OS != "Linux" {
		t.Fatalf("%v %+v", err, check)
	}

	// The restricted key behind an observer may run only fixed commands, so
	// preflight must send exactly one: no compound probe, no tmux/git/claude.
	if cmds := host.Commands(); len(cmds) != 1 || cmds[0] != "uname -s" {
		t.Errorf("an observer preflight must be exactly `uname -s`, sent %q", cmds)
	}
}

func TestObserverIsNotRequiredToHaveTmuxGitOrClaude(t *testing.T) {
	f, host := newTestFleet(t)
	host.missingTools["tmux"], host.missingTools["git"], host.missingTools["claude"] = true, true, true

	if check, err := f.Check(context.Background(), "server"); err != nil || !check.OK {
		t.Fatalf("an observer without tmux/git/claude must pass: %v %+v", err, check)
	}
}

func TestObserverIsNeverALaunchOrRefreshOrLoginTarget(t *testing.T) {
	f, _ := newTestFleet(t)
	ctx := context.Background()

	if _, err := f.ResolveTarget("server", ""); err == nil {
		t.Error("an observer must not resolve as a launch target")
	}

	if _, err := f.AuthLoginStart(ctx, "server"); err == nil {
		t.Error("there is no claude to log in on an observer")
	}

	got, _ := f.PathsStatus(ctx, "", "", true)
	for _, e := range got {
		if e.Host == "server" {
			t.Error("an observer has no paths to report")
		}
	}
}
