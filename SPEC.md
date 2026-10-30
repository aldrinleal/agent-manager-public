# agent-manager — spec

A small fleet manager for remote coding-agent sessions. It holds an SSH
private key, reaches a list of Linux hosts, launches `claude` inside `tmux`
in a given repo/worktree, scrapes the Remote Control URL out of the session,
and keeps a registry of what is running where. It also does plain git
refreshes (fetch/merge/rebase) on those same repos.

Written in Go, modelled on `../api-kit` (gin + logrus + docopt + errorx,
top-level feature packages, `cmd/<tool>/main.go`, `CGO_ENABLED=0` static
binary) with `../notes-svc`'s operational shape (config from env, sqlite state,
a Makefile, a scratch container image).

This is deliberately **not** OpenHands, not a sandbox, not a scheduler. It is
the "registry + launcher" that `need.md` concludes you have to build yourself
because no vendor exposes one.

---

## 1. Scope

### In scope (v1)

- An inventory of **hosts** (`user@host[:port]`, optional jump host) and, per
  host, a list of **paths** (git checkouts) it is allowed to touch.
- **Launch** a `claude-code` session on `host` + `path`, in a dedicated git
  worktree and branch, inside a persistent `tmux` session.
- **Capture** the `https://claude.ai/code/...` Remote Control URL emitted at
  launch and persist it.
- **Reconcile** session liveness independently of the URL (`tmux has-session`,
  pane pid/dead status, log mtime).
- **Git refresh** a path: fetch + `ff-only` / `rebase` / `merge` / `fetch-only`,
  with a before/after SHA report.
- Three surfaces over one core: **CLI**, **HTTP API** (bearer token), and a
  **minimal web console** (one embedded HTML page, no build step).
- **sqlite** registry (pure-Go driver) of every session ever launched.

### Deferred (designed for, not built)

- `kubectl` transport (agents inside pods). The `Transport` interface in §5 is
  the seam; nothing else in the design assumes SSH. Design note in §5.5.
- `cline`, `codex`, `grok` runners. The `Runner` interface in §7 is the seam —
  a runner is a launch-script template plus a URL regex.
- PR creation, auto-merge, cost/token accounting.

### Non-goals

- Sandboxing or isolating what the agent does on the host. The agent runs as
  the SSH user, with that user's full rights. Isolation is the host's job (a
  dedicated unix account, a VM, or — later — a pod).
- Shipping model credentials. `claude` must already be authenticated on each
  host. The manager's key gets it *onto* the host; nothing more.

---

## 2. Remote host requirements

Minimum, checked by preflight (§8):

| Requirement | Checked with | Why |
|---|---|---|
| Linux | `uname -s` | tmux/paths/`/proc` assumptions |
| `tmux` | `tmux -V` | session persistence + log capture + status |
| `git` | `git --version` | worktrees + refresh |
| `claude` on PATH | `claude --version` | only for hosts that run the `claude-code` runner |

Notably **not** required: `script(1)`, `base64`, `bash`, an SFTP subsystem.
Log capture uses `tmux pipe-pane` (§7.4) precisely so the stated minimum holds.

---

## 3. Layout

```
agent-manager/
├── cmd/
│   ├── agentmgr/main.go        # CLI (docopt)
│   └── agentmgrd/main.go       # server: gin API + console (api-kit's main.go shape)
├── config/                     # inventory + settings
│   ├── config.go               # Settings, env-loaded, package-level singleton
│   └── inventory.go            # hosts.yaml -> Host/Path structs, validation
├── sshx/                       # SSH transport
│   ├── key.go                  # private key loading (file/env/agent, passphrase)
│   ├── hostkey.go              # known_hosts verification + `trust` flow
│   ├── client.go               # dial (incl. jump host), pooled per host
│   └── exec.go                 # Run / RunStdin / WriteFile — implements Transport
├── tmux/                       # tmux command building + output parsing
├── gitx/                       # refresh strategies, worktree add/remove, status
├── runner/
│   ├── runner.go               # Runner interface + registry
│   └── claudecode.go           # launch script template + URL regex
├── registry/                   # sqlite (modernc.org/sqlite), sessions + host_checks
├── fleet/                      # the orchestrator — the only thing CLI/API call
├── api/
│   ├── group.go                # NewFleetGroup("/api/fleet/v1", engine, mw...)
│   └── handler.go
├── console/
│   ├── console.go              # go:embed
│   └── index.html
├── util/                       # EnvIf, shquote, ansi-strip (api-kit's util/)
├── k8s/                        # configmap / secret / deployment / service
├── hosts.example.yaml
├── .env.example
├── Containerfile
├── Makefile
└── SPEC.md / README.md / CLAUDE.md
```

Module: `github.com/aldrinleal/agent-manager-public`, Go 1.25.

Top-level feature packages rather than `internal/` — matches api-kit. No
`internal/` means the packages are importable from other tools of yours (e.g.
OpenClaw dispatching into `fleet` directly instead of over HTTP).

### Dependencies

Everything already proven in your tree:

```
github.com/gin-gonic/gin              # api-kit
github.com/gin-contrib/cors           # api-kit
github.com/sirupsen/logrus            # api-kit + cli-kit
github.com/joomcode/errorx            # api-kit + cli-kit
github.com/docopt/docopt.go           # api-kit + cli-kit (CLI)
github.com/google/uuid                # api-kit
golang.org/x/crypto/ssh               # (+ ssh/knownhosts, ssh/agent)
golang.org/x/term                     # interactive passphrase prompt (CLI only)
modernc.org/sqlite                    # ALREADY an indirect dep of api-kit
gopkg.in/yaml.v3                      # inventory
```

> **`modernc.org/sqlite`, not `mattn/go-sqlite3`.** The Makefile builds with
> `CGO_ENABLED=0` (api-kit's recipe, and what makes the Alpine/scratch image
> work). A cgo sqlite driver silently breaks that build line.

---

## 4. Configuration

Two things, deliberately separate: **settings** (env, secrets, deployment
knobs) and **inventory** (hosts/paths, a YAML file you edit and commit).

### 4.1 Settings — env only (`config/config.go`)

Package-level singleton `config.Settings`, loaded once, notes-svc's pattern.

| Var | Default | Purpose |
|---|---|---|
| `AGENTMGR_INVENTORY` | `hosts.yaml` | path to the inventory file |
| `AGENTMGR_DB_PATH` | `agentmgr.db` | sqlite registry |
| `AGENTMGR_API_TOKEN` | *(empty)* | bearer token; **empty ⇒ API refuses to start** (fail closed, notes-svc §api.py) |
| `AGENTMGR_PORT` | `8000` | gin bind port |
| `AGENTMGR_SSH_KEY_FILE` | `~/.ssh/id_ed25519` | private key path |
| `AGENTMGR_SSH_KEY` | *(empty)* | private key **contents** (PEM); wins over the file — this is the k8s Secret path |
| `AGENTMGR_SSH_KEY_PASSPHRASE` | *(empty)* | for an encrypted key |
| `AGENTMGR_SSH_USE_AGENT` | `true` | also offer `$SSH_AUTH_SOCK` identities |
| `AGENTMGR_KNOWN_HOSTS` | `~/.ssh/known_hosts` | host key verification |
| `AGENTMGR_SSH_INSECURE_HOST_KEY` | `false` | skip host key checks; logs a warning on **every** dial |
| `AGENTMGR_SSH_TIMEOUT_S` | `20` | dial + handshake timeout |
| `AGENTMGR_EXEC_TIMEOUT_S` | `60` | per-command timeout (git ops get their own, longer) |
| `AGENTMGR_URL_SCRAPE_TIMEOUT_S` | `120` | how long to wait for the Remote Control URL |
| `AGENTMGR_RECONCILE_INTERVAL_S` | `30` | background sweep; `0` disables |
| `AGENTMGR_STALE_AFTER_S` | `1800` | no log growth for this long ⇒ `stale` |
| `AGENTMGR_MAX_CONCURRENT_LAUNCHES` | `4` | semaphore around launch |

### 4.2 Inventory — `hosts.yaml`

```yaml
defaults:
  identity: ~/.ssh/id_ed25519
  runner: claude-code
  worktree_root: /srv/agent-workspaces     # per-host overridable
  log_root: /srv/agent-logs
  refresh: ff-only

hosts:
  # the simple case — "username@host is enough"
  - name: devbox
    address: dev@devbox
    paths:
      - name: customer-portal
        dir: /home/dev/code/customer-portal
        remote: origin
        branch: main

  # reached only through a bastion,
  # on port 2200, as a different user.
  - name: homelab
    address: dev@localhost:2200
    jump: bastion                      # name of another entry below
    identity: ~/.ssh/id_ed25519
    env:
      PATH: /home/dev/.local/bin:/usr/local/bin:/usr/bin:/bin
    worktree_root: /srv/agent-workspaces
    paths:
      - name: notes-svc
        dir: /home/dev/src/notes-svc
      - name: api-kit
        dir: /home/dev/src/api-kit
        branch: master

  - name: bastion
    address: ubuntu@bastion.example.net
    role: jump                             # not a launch target; no paths needed
```

**Connection method** is the `address` field: `user@host`, optionally
`:port`, optionally reached through `jump: <other host name>`. That is the
whole model — it covers `ssh devbox` and
`ssh -J ubuntu@bastion -p 2200 dev@localhost` equally, and it maps 1:1
onto how `x/crypto/ssh` chains connections (§5.2).

**Paths** are an allowlist. `fleet` refuses any `dir` not named in the
inventory — the API never takes a free-form remote path. (need.md's "não deixe
o agente receber um comando shell arbitrário", enforced structurally.)

Per-path optional fields: `remote` (default `origin`), `branch` (default
`main`), `refresh` (strategy override), `runner`, `allow_dirty` (default
`false`), `workspace` (`worktree` | `inplace`, default `worktree`).

---

## 5. SSH — how the key is held and used

### 5.1 The key

Resolution order, first hit wins:

1. `AGENTMGR_SSH_KEY` (PEM contents) — the deployed path; comes from a k8s
   Secret, never touches disk.
2. Host's `identity:` / `defaults.identity` / `AGENTMGR_SSH_KEY_FILE`.
3. `$SSH_AUTH_SOCK` agent identities, if `AGENTMGR_SSH_USE_AGENT`.

Encrypted keys go through `ssh.ParsePrivateKeyWithPassphrase` with
`AGENTMGR_SSH_KEY_PASSPHRASE`; the **CLI** may prompt interactively via
`golang.org/x/term` when the env var is absent and stdin is a terminal. The
**server never prompts** — it fails to start instead.

The parsed `ssh.Signer` lives in memory for process lifetime. It is never
logged, never persisted to the registry, never returned by any API route, and
the `/hosts` response carries only the key's *fingerprint*, not the key.

> **Use a dedicated key.** This process is, by construction, a remote-code-
> execution service for every host in the inventory. Generate
> `~/.ssh/id_agentmgr`, authorize it for a dedicated unix user on each host,
> and keep the API on Tailscale behind the bearer token. Do not hand it your
> personal `id_ed25519` and then expose port 8000.

### 5.2 Dialing, including the jump host

No `ssh` binary, no `~/.ssh/config` parsing. Chaining is explicit:

```
dial(host):
    if host.jump != "":
        jc   := dial(inventory[host.jump])              # recursive, depth-capped
        conn := jc.Dial("tcp", host.addr)               # tunnelled TCP
        c, ch, rq := ssh.NewClientConn(conn, host.addr, cfg)
        return ssh.NewClient(c, ch, rq)
    return ssh.Dial("tcp", host.addr, cfg)
```

Clients are pooled per host (`map[string]*ssh.Client` + mutex), with keepalive
(`SendRequest("keepalive@openssh.com", ...)` every 30s) and transparent redial
on a dead connection. A launch sweep of 30 sessions across 3 hosts uses 3
connections, not 30.

### 5.3 Host keys

`knownhosts.New(AGENTMGR_KNOWN_HOSTS)` by default — **strict**.
`AGENTMGR_SSH_INSECURE_HOST_KEY=true` exists for a first run but logs a
warning on every dial. To onboard a host properly:

```
agentmgr hosts trust homelab     # dials, prints the fingerprint, asks, appends
```

### 5.4 The transport seam

```go
type Result struct { Stdout, Stderr string; ExitCode int }

type Transport interface {
    Run(ctx context.Context, cmd string) (Result, error)
    RunStdin(ctx context.Context, cmd string, stdin io.Reader) (Result, error)
    WriteFile(ctx context.Context, path string, mode os.FileMode, r io.Reader) error
    Close() error
}
```

`sshx.Client` implements it today; a `kubectlx.Client` (`kubectl exec -i`)
implements it later with no change above this line. Nothing in `tmux/`,
`gitx/`, `runner/` or `fleet/` imports `sshx` directly.

`WriteFile` is `RunStdin(ctx, "cat > 'path' && chmod ...", r)` — writing bytes
straight down the session's stdin. **No quoting of file contents, ever**, and
no dependency on SFTP or `base64`.

### 5.5 The `kubectl exec` transport — transport built, workspaces not yet

**Status 2026-10-29:** the inventory field, the seam change and `kubectlx`
are built and verified against a throwaway pod (not included in this edition). The workspace
image, its StatefulSet and the RBAC grant are not — see
`docs/HANDOVER-2026-11-kubectl.md` §6–§7. What follows is the design as
written before it was built; where the build departed from it, it says so.

For agents in pods (the per-language workspace StatefulSets in
`k8s-descriptors/TODO.md` §9). Written down so the shape is agreed before
anyone types `client-go` into `go.mod`.

**tmux stays. The transport is not what keeps the agent alive.** This is
already true for SSH and is the reason tmux is in the design at all: the
transport only ever carries short commands — `tmux new-session -d …`,
`capture-pane`, `git status`. The agent's parent is the tmux server, which is
daemonised and owned by the host, not by the connection. The manager being
restarted (a rollout), the SSH connection dropping, or the API server being
unreachable for a minute kills nothing; the registry on the PVC still has the
session name and the next reconcile sweep re-attaches by it (§9). An exec
transport must keep that property, which means one rule: **`claude` is never
the exec'd process.** Exec'ing `claude` directly would tie its lifetime to a
SPDY stream from a pod that gets rolled on every deploy. What *does* kill a
session in a pod is the pod dying — eviction, OOM, node reboot — which is the
same class of event as a laptop rebooting: the worktree survives on the PVC,
the tmux server does not, reconcile marks the session `dead`.

**Native client, not the binary.** `k8s.io/client-go/tools/remotecommand`
(SPDY, WebSocket on newer servers) — the same principle as `x/crypto/ssh`
over an `ssh` binary: the image stays `FROM scratch`, there is nothing to
exec in it. The cost is that client-go is a large dependency; a separate
package (`kubectlx`) keeps it out of every build that does not want it.
*As built:* `kubectlx` uses a bare `rest.RESTClient` over a scheme holding
only `core/v1`, not the typed `Clientset` — the Clientset registers every API
group and took the binaries from 10/27 MB to 31/63 MB; this way it is
20/36 MB. Executor is `NewFallbackExecutor(websocket, spdy)`, as kubectl does.

**Mapping onto `Transport`:**

| Method | kubectl |
|---|---|
| `Run(cmd)` | exec `["/bin/sh","-c",cmd]`, stdout/stderr captured; exit code from `exec.CodeExitError`, a non-zero exit is a `Result`, not an error, same as sshx |
| `RunStdin` / `WriteFile` | the same with a `Stdin` stream — content still never quoted, never shell-interpreted; needs `/bin/sh` and `cat` in the workspace image, which it has anyway |
| `wrap()` env export | unchanged, it is part of the command string |
| `Close` | nothing to close — there is no persistent connection to pool; every call is a new stream, ~100–300 ms. Reconcile is already one sweep command per host (§9), a launch is a handful, so this is fine |

**The gotcha to verify live before trusting it (not included in this edition):** a
`remotecommand` stream ends when stdout and stderr are closed by *every*
process holding them. `tmux new-session -d` forks a server that `daemon()`s
and redirects its fds to `/dev/null`, so the exec should return; but if
anything in the launch chain inherits the exec's fds (`pipe-pane` runs under
the server, so it should not), `Run` hangs until the context deadline. The
existing per-call timeouts bound the damage; the live check is a throwaway
launch and `time`.

**Inventory:** a `transport: kubectl` field (default `ssh`), plus `namespace`,
`pod` (a StatefulSet's is `<name>-0`) and optional `container`. `address`,
`jump` and `known_hosts` do not apply and validation rejects them on such a
host. Roles and `paths` are unchanged. There is **no DNS at all** on this
path.

**The one real code change above the seam** (*done*: `fleet.Dialers`;
`fleet` no longer imports `sshx`): `fleet` used to take a
`*sshx.Pool` (`fleet.go`, `New` and `Pool()`), which is the seam leaking by one
type. `transports` is already a `func(host) (Transport, error)` field, so the
fix is a per-host factory built in `cmd/` from the `transport:` field, with
`fleet.New` taking that instead of the pool. `tmux/`, `gitx/`, `runner/` are
untouched — they were the point of §5.4.

**Access = RBAC, not a key.** The pod's service account gets `create` on
`pods/exec` and `get` on `pods` in a dedicated namespace (`workspaces`), via a
Role there, nothing cluster-wide. Be clear about what that is: *run anything
in any pod of that namespace*, the exact equivalent of the SSH key, and it
belongs in the security posture list. The StatefulSet today has no
`serviceAccountName`; it would need one, and the token mounted.

**What the workspace image needs:** `sh`, `cat`, `tmux`, `git`, node +
`claude`, and a PID 1 that reaps (`tini`): the tmux server is re-parented to
it when the exec'd shell exits, and a PID 1 that does not `wait()` leaks a
zombie per finished agent. No `sshd`: with this transport it buys nothing, and
`kubectl exec -it <pod> -- tmux attach -t <session>` is the `attach` command
(§11 prints the `ssh` form today; it prints this one for a kubectl host).

**Preflight, launch script, URL scrape, log capture, refresh:** unchanged,
they are command strings over a `Transport`.

---

## 6. Registry (sqlite)

`registry/`, pure-Go `modernc.org/sqlite`, file at `AGENTMGR_DB_PATH`.

```sql
CREATE TABLE sessions (
  id              TEXT PRIMARY KEY,      -- uuid
  task_id         TEXT NOT NULL,         -- human slug, unique per (host, repo)
  runner          TEXT NOT NULL,         -- 'claude-code'
  host            TEXT NOT NULL,
  repo_name       TEXT NOT NULL,
  repo_dir        TEXT NOT NULL,
  worktree        TEXT NOT NULL,
  branch          TEXT NOT NULL,
  tmux_session    TEXT NOT NULL,
  log_path        TEXT NOT NULL,
  remote_url      TEXT,                  -- https://claude.ai/code/...
  state           TEXT NOT NULL,         -- see §9
  pane_pid        INTEGER,
  exit_status     INTEGER,
  prompt_preview  TEXT,                  -- first ~200 chars, for the console
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  last_seen_at    TEXT,                  -- last successful reconcile
  ended_at        TEXT,
  note            TEXT                   -- failure reason / operator note
);
CREATE UNIQUE INDEX sessions_key ON sessions(host, repo_name, task_id);
CREATE INDEX sessions_state ON sessions(state);

CREATE TABLE host_checks (               -- preflight cache
  host          TEXT PRIMARY KEY,
  ok            INTEGER NOT NULL,
  os            TEXT,
  tmux_version  TEXT,
  git_version   TEXT,
  runner_version TEXT,
  note          TEXT,
  checked_at    TEXT NOT NULL
);
```

Serialized through a single `*sql.DB` with `_pragma=journal_mode(WAL)` and
`SetMaxOpenConns(1)` — personal-scale, same call notes-svc made.

---

## 7. Runners

```go
type Runner interface {
    Name() string
    Preflight() []Probe                       // extra commands beyond linux/tmux/git
    LaunchScript(spec LaunchSpec) string      // the #! script written to the host
    URLPattern() *regexp.Regexp               // nil ⇒ this runner has no URL
}
```

v1 registers exactly one: `claude-code`.

### 7.1 The launch script — why a script, not a `tmux new-session "..."`

Passing a command to `tmux new-session` puts it through tmux's own parser and
then a shell — two layers of quoting over a user-supplied prompt. Instead:

1. Write the prompt to `<log_root>/<repo>/<task>.prompt.md` (`WriteFile`, raw
   stdin) — beside the log and launch script, **not** in the worktree.
2. Render a launch script and write it to
   `<log_root>/<repo>/<task>.launch.sh`, mode `0700`.
3. `tmux new-session -d -s <session> -c <worktree> <script-path>`

Only *paths we generate and sanitize* are ever quoted. The prompt never passes
through a shell at all.

```sh
#!/bin/sh
# rendered by agent-manager; host={{.Host}} task={{.TaskID}}
{{range $k, $v := .Env}}export {{$k}}={{shq $v}}
{{end}}
cd {{shq .Worktree}} || exit 90
exec claude --remote-control {{shq .SessionLabel}} "$(cat {{shq .PromptFile}})"
```

> **PATH gotcha.** `claude` is usually a per-user npm/`~/.local/bin` install,
> and a non-interactive `tmux`-spawned `/bin/sh` does not read `.bashrc`.
> Hence the explicit per-host `env: PATH:` in the inventory, exported at the
> top of the script. Preflight runs `command -v claude` through the *same*
> env so a mismatch is caught at `agentmgr check`, not at 2am.

### 7.2 Session naming

`agent-<repo>-<task>`, with every character outside `[A-Za-z0-9_-]` collapsed
to `-` (tmux treats `.` and `:` specially). Collision with an existing tmux
session ⇒ the launch is rejected and the existing registry row is returned
(idempotent re-launch), unless `--force`.

### 7.3 Workspace

`workspace: worktree` (default):

```
git -C <repo_dir> fetch --prune <remote>
git -C <repo_dir> worktree add -B agent/<task> <worktree_root>/<repo>/<task> <remote>/<branch>
```

`workspace: inplace` runs in `repo_dir` itself — for single-agent boxes or
repos where worktrees break the toolchain (vendored node_modules, submodules).
need.md is right that worktrees are the default; `inplace` is the escape hatch.

### 7.4 Log capture — `pipe-pane`, not `script(1)`

```
tmux pipe-pane -o -t <session> "cat >> '<log_path>'"
```

Uses only tmux, so the stated minimum (linux/tmux/git) really is the minimum.
`script -q -f` remains available as `capture: script` per host for the case
where you want the pty-accurate stream.

Immediately after creating the session:

```
tmux set-option -t <session> remain-on-exit on
```

so a finished agent leaves a *dead but visible* pane. Reconciliation then
reads `#{pane_dead}` / `#{pane_dead_status}` for a real exit code, instead of
need.md's `; printf ...; read` trick.

### 7.5 URL scrape

Pattern: `https://claude\.ai/code/[A-Za-z0-9_/?=&.-]+`

Polled until `AGENTMGR_URL_SCRAPE_TIMEOUT_S` (2s interval, jittered) against
**both** sources, in one round trip per poll:

```
tmux capture-pane -p -J -S -200 -t <session> ; tail -c 65536 '<log_path>'
```

- `-J` joins wrapped lines. **Without it a long URL wrapped at the terminal
  width is scraped truncated** — the single most likely silent bug in this
  whole design.
- ANSI escapes are stripped (`util/ansi.go`) before matching.
- First match wins and is persisted. Failure to find a URL is **not** a launch
  failure — the session stays `running` with `remote_url = NULL`; you still
  have tmux attach and the log. (need.md: status must not depend on the URL.)

---

## 8. Preflight

`agentmgr check [<host>]` / `POST /hosts/{host}/check`, cached in
`host_checks` with a TTL (re-run on cache miss before any launch).

One SSH connection, one compound command:

```sh
uname -s; tmux -V; git --version; command -v claude && claude --version
```

Any missing piece produces a specific message —
`homelab: tmux not found (need: linux, tmux, git)` — not a generic failure.

---

## 9. Reconciliation and state

States: `starting` → `running` → (`stale` | `stopped` | `failed`).

| State | Meaning |
|---|---|
| `starting` | launch script written, tmux session created, URL not yet found |
| `running` | tmux session exists, pane alive |
| `stale` | session alive but log has not grown for `AGENTMGR_STALE_AFTER_S` |
| `stopped` | pane dead (`#{pane_dead}`) or session gone; `exit_status` recorded |
| `failed` | launch itself errored (git, tmux, preflight) — `note` says which |

The sweep runs every `AGENTMGR_RECONCILE_INTERVAL_S` in the server (a
goroutine started by `fleet`), and on demand for `agentmgr ls`.

**One round trip per host, not per session:**

```sh
tmux list-panes -a -F '#{session_name} #{pane_pid} #{pane_dead} #{pane_dead_status} #{pane_current_command}'
```

plus a single `stat`-equivalent over the host's log directory. Sessions in the
registry that do not appear in that output are `stopped`.

---

## 10. Git refresh

Independent of any agent session — this is the "keep my checkouts current"
half of the tool.

`agentmgr refresh <host>[/<path>] [--strategy=...]` /
`POST /api/fleet/v1/refresh`

Strategies: `ff-only` (default) | `rebase` | `merge` | `fetch-only`.

```
git -C <dir> rev-parse --is-inside-work-tree     # guard
git -C <dir> status --porcelain                   # dirty ⇒ refuse (unless allow_dirty/--force)
git -C <dir> rev-parse HEAD                       # before
git -C <dir> fetch --prune <remote>
  ff-only  : git -C <dir> merge --ff-only <remote>/<branch>
  rebase   : git -C <dir> rebase <remote>/<branch>
  merge    : git -C <dir> merge --no-edit <remote>/<branch>
  fetch-only: (stop here)
git -C <dir> rev-parse HEAD                       # after
git -C <dir> diff --stat <before> <after>
```

Explicitly `fetch` + `merge <remote>/<branch>` rather than `git pull`, so it
does not depend on upstream tracking being configured, and works on a detached
or oddly-tracked checkout.

A failed rebase/merge leaves the repo mid-operation on purpose — the result
reports `conflict: true` plus the conflicted paths, and refuses to `--abort`
on your behalf.

> **Git credentials belong to the host.** The manager's key authenticates *to*
> the host; the host's own deploy key / credential helper authenticates to
> GitHub. The manager never forwards an agent and never injects a token.

---

## 11. CLI (`cmd/agentmgr`, docopt — cli-kit/api-kit style)

```
agentmgr hosts [--json]
agentmgr hosts trust <host>
agentmgr check [<host>]
agentmgr up <host>/<path> <task-id> [--prompt=<p>|--prompt-file=<f>] [--branch=<b>] [--inplace] [--force]
agentmgr ls [--host=<h>] [--state=<s>] [--json]
agentmgr url <task-id>
agentmgr log <task-id> [--tail=<n>] [--follow]
agentmgr send <task-id> <text>
agentmgr attach <task-id>              # prints the ssh+tmux command to run
agentmgr kill <task-id> [--remove-worktree]
agentmgr refresh <host>[/<path>] [--strategy=<s>] [--force]
agentmgr serve [--port=<p>]
```

`agentmgr up` prints the registry row and blocks (with a spinner) until the
URL appears or the scrape times out — then exits either way.

## 12. HTTP API (`api/`, gin, `/api/fleet/v1`)

Root engine carries `GET /` (console), `GET /healthz`, `GET /__dump` —
api-kit's built-ins. Everything under `/api/fleet/v1` requires
`Authorization: Bearer $AGENTMGR_API_TOKEN`; an unset token makes the server
refuse to start.

| Method | Route | Purpose |
|---|---|---|
| `GET` | `/hosts` | inventory + cached preflight + key fingerprint |
| `POST` | `/hosts/{host}/check` | re-run preflight |
| `GET` | `/sessions` | list; `?host=&repo=&state=` |
| `POST` | `/sessions` | launch; `202` + row, URL filled in asynchronously |
| `GET` | `/sessions/{id}` | one row |
| `GET` | `/sessions/{id}/log?tail=200` | tail (capture-pane + log) |
| `POST` | `/sessions/{id}/send` | `tmux send-keys -t <s> -l <text>` + `Enter` |
| `DELETE` | `/sessions/{id}?remove_worktree=true` | kill session, optionally drop the worktree |
| `POST` | `/refresh` | git refresh, returns before/after SHAs + diffstat |

`POST /sessions` body:

```json
{ "host": "homelab", "path": "notes-svc", "task_id": "fix-cookie-refresh",
  "prompt": "...", "branch": "main", "workspace": "worktree" }
```

Registered exactly like api-kit's feature packages:
`api.NewFleetGroup("/api/fleet/v1", engine, authMiddleware)`.

## 13. Console (`console/`, `go:embed`)

One `index.html`, vanilla JS + `fetch`, no npm, no build step. Served at `/`,
polls `GET /sessions` every 5s.

```
TASK                      RUNNER       HOST      REPO        BRANCH               STATE    AGE
fix-cookie-refresh        claude-code  homelab   notes-svc    agent/fix-cookie…    running  12m   [open] [log] [kill]
bump-gin                  claude-code  devbox    api-kit    agent/bump-gin       stale    4h    [open] [log] [kill]
```

`[open]` follows `remote_url` (disabled when null), `[log]` opens a tail
panel, `[kill]` confirms then `DELETE`s. A "Refresh repo" control per host/path
hits `POST /refresh`. Token comes from a field stored in `localStorage` —
adequate behind Tailscale, and stated as such.

---

## 16. Gotchas to not re-discover

Everything below was hit for real while building and testing this against
`laptop-b.local` and `laptop-a.local`, except where marked.

### tmux targeting — the two that fail silently

1. **`remain-on-exit` is a *window* option, not a session option.**
   `tmux set-option -t '=sess' remain-on-exit on` fails with
   `no such window: =sess`. It needs `-w` and a window target:
   `set-option -w -t '=sess:' remain-on-exit on`. It is chained onto
   `new-session` with a literal `';'` in the same command, so a fast-failing
   agent cannot exit before the option applies.

2. **Pane-target commands need `=session:`, with the trailing colon.**
   `pipe-pane` and `send-keys` given a bare `=session` fail with
   `can't find pane`. `capture-pane` is worse: it **returns empty output
   instead of failing**, so with `2>/dev/null` in the command the URL scrape
   would simply never match and nothing would ever say why. `tmux/tmux.go`
   keeps `target()` (session commands) and `paneTarget()` (pane/window
   commands) separate for exactly this reason.

3. **`capture-pane` without `-J`** truncates a Remote Control URL wrapped at
   the terminal width. (Anticipated, not hit — because of `-J`.)

### Terminal output

4. **Strip ANSI with the full ECMA-48 CSI class, `[0-?]`, not `[0-9;?]`.**
   Claude Code's TUI emits private-parameter sequences — `ESC[<u`, `ESC[>5u`,
   `ESC[>4;2m`, `ESC[>0q` — whose prefixes are `<`, `=`, `>` (0x3C–0x3E), not
   just `?`. A narrower class leaves `[<u` and friends as visible garbage in
   every captured line.

5. **Never read a live session by tailing the `pipe-pane` log.** For a
   full-screen TUI the log is a stream of terminal *redraws*, not a
   transcript: tailing it yields the same few sentences re-emitted dozens of
   times, interleaved with spinner frames. `capture-pane` returns the
   rendered screen plus scrollback, which is what a human wants.
   `fleet.Log` uses capture-pane for live sessions and falls back to the log
   file only once tmux no longer has the session. The log still earns its
   keep: it outlives the session, and it is a second source for the URL scrape
   when the banner has scrolled off.

### SSH

6. **Pin `HostKeyAlgorithms` to what `known_hosts` already has.**
   `x/crypto/ssh` negotiates by its own preference order, which is not
   OpenSSH's — a host `ssh` connects to happily can negotiate a different key
   type here and read as a **key mismatch**. `sshx.hostKeys.algorithms()`
   probes the callback with an impossible key and reads `KeyError.Want` to
   learn the pinned algorithms. A pinned `ssh-rsa` key also offers
   `rsa-sha2-256/512`, since a modern sshd refuses SHA-1.

7. **Load the key eagerly, not on first dial.** Lazy loading means
   `agentmgr hosts` and `GET /hosts` report no key at all until something
   happens to connect, and a bad key or wrong passphrase surfaces at the worst
   possible moment instead of at startup.

8. **Pool SSH clients per host.** A connection per command turns the reconcile
   sweep into a self-inflicted DoS on sshd's `MaxStartups`. (Anticipated.)

9. **`x/crypto/ssh` will happily skip host key checking if you let it.**
   `InsecureIgnoreHostKey` must be an explicit, loudly-logged opt-in
   (`--insecure-host-key` / `AGENTMGR_SSH_INSECURE_HOST_KEY`), never the
   fallback when verification errors.

### Launch

10. **`claude` is not on a non-interactive shell's PATH.** It is typically a
    `~/.local/bin` or npm install, and a `tmux`-spawned `/bin/sh` reads no rc
    file. Hence per-host `env: PATH:`, exported at the top of the launch
    script — and preflight probes through the *same* env, so the failure lands
    on `agentmgr check` rather than as an opaque exit 127 at 2am. Confirmed on
    both real hosts.

11. **Never pass the prompt through a shell.** Write it to a file over the
    session's raw stdin and `cat` it in the script. tmux's parser plus a shell
    is two layers of quoting over user input. Verified against a prompt
    containing `` ` ``, `$( )`, `'` and `"` — it arrives byte-exact and appears
    nowhere in the generated script.

12. **Never write anything into the worktree.** The prompt file originally
    landed at `<worktree>/.agent-prompt.md`, which leaves an untracked file
    that makes `git status` dirty *forever*: it blocks `agentmgr refresh` on an
    in-place path, and makes every worktree look like it holds work when it
    does not. It now lives under `log_root` beside the log, which also means it
    survives `git worktree remove` and stays auditable.

13. **Clean up after a partial launch.** If anything after `new-session`
    fails, kill the session. A live agent recorded as `failed` is worse than
    no agent: nothing watches it, and the next launch of that task id just
    conflicts.

### Everything else

14. **`modernc.org/sqlite`, not `mattn/go-sqlite3`** — the Makefile builds
    `CGO_ENABLED=0`, and a cgo driver silently breaks that line.

15. **Don't use docopt's `[options]` shortcut.** docopt-go excludes from it
    any flag that also appears explicitly in another usage line, which
    silently turned `agentmgr up --force` into a usage error while every other
    flag worked. List options per command.

16. **Untracked files must not block a git refresh.** `git status --porcelain`
    counts `??` entries; almost every real checkout has stray build output, so
    the dirty guard passes `--untracked-files=no`. An untracked file cannot be
    lost by a fetch or fast-forward, and if one really would be overwritten,
    git refuses on its own with a better message.

17. **`enableServiceLinks: false`.** Kubernetes injects `<SERVICE_NAME>_PORT`
    for every Service in the namespace. `AGENTMGR_*` does not collide with
    `AGENT_MANAGER_*`, so this is precautionary — but notes-svc lost an
    afternoon to exactly this, and it is free.

---

