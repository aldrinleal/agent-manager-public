# agent-manager HTTP API

Base: `${AGENTMGR_URL:-http://localhost:8000}/api/fleet/v1`

Every route needs `Authorization: Bearer $AGENTMGR_API_TOKEN`. Only
`GET /healthz` (at the root, not under the prefix) and the web console are open.

An **unset** token makes the server refuse *every* request with 500, not allow
them — it fails closed on purpose. A 401 means the token is wrong; a 500
mentioning `AGENTMGR_API_TOKEN` means the server was started without one.

## Routes

| Method | Path | Notes |
|---|---|---|
| `GET` | `/hosts` | inventory, cached preflight, SSH key **fingerprints** (never the key) |
| `POST` | `/hosts/{host}/check` | re-run preflight now; a *failed* check still returns 200 with `error` set |
| `GET` | `/hosts/{host}/auth` | last known `claude auth status`: logged in?, account, plan |
| `POST` | `/hosts/{host}/auth/login` | start `claude auth login` in tmux, return the OAuth URL (`{url, reused, expires_in}`) |
| `POST` | `/hosts/{host}/auth/code` | `{"code": "..."}` — type it into the waiting login; success = the login process exited 0 **and** status is logged in |
| `DELETE` | `/hosts/{host}/auth/login` | abandon a waiting login (idempotent) |
| `GET` | `/history/projects` | `?days=90` — every worked-on project ranked by sessions, resolved against the launch hosts (exists / git / commits / default branch / allowlisted) |
| `GET` | `/history/proposals` | `?days=90` — allowlist verdicts + paste-ready `yaml`; `unreachable` lists hosts that did not answer. **Never edits the allowlist** |
| `GET` | `/history/worklog` | `?host=&days=7&tz=` — by local day, project, session, with prompt text when stored |
| `GET` | `/history/pulls` | the last read of each host's history: bytes, parsed, new, note |
| `POST` | `/history/pull` | `{"host": "..."}` or `{}` for every host; idempotent |
| `GET` | `/paths` | every allowlisted checkout with its git state; `?host=&path=&fresh=true`. One SSH round trip per host, cached 60s |
| `POST` | `/sessions/{id}/push/plan` | what pushing the session's `agent/<task>` branch would send, what it would not, and why it would be blocked |
| `POST` | `/sessions/{id}/push` | push it with `-u`; returns git's output and the pull-request link. **No force option exists** |
| `GET` | `/sessions/{id}/diff` | worktree state + commits ahead of the base + diffstat against the fork point |
| `GET` | `/sessions` | `?host=&repo=&task_id=&state=&active=true&limit=&reconcile=true` |
| `POST` | `/sessions/plan` | what a launch **would** do — same body as `/sessions`, changes nothing |
| `POST` | `/sessions` | launch; returns **202**, URL arrives later |
| `GET` | `/sessions/{id}` | one session |
| `GET` | `/sessions/{id}/log?tail=N` | rendered screen for a live session, log file for a dead one |
| `GET` | `/sessions/{id}/attach` | `ssh` + `tmux attach` commands, worktree and log paths |
| `POST` | `/sessions/{id}/send` | `{"text": "..."}` — typed into the pane, then Enter |
| `DELETE` | `/sessions/{id}?remove_worktree=true` | worktree kept unless asked |
| `POST` | `/refresh/plan` | what a refresh **would** do, per path, with dirty state — same body as `/refresh` |
| `POST` | `/refresh` | `{host, path?, strategy?, force?, autostash?}` — omit `path` for every repo on the host |

`{id}` accepts a session id or an unambiguous task id.

`?reconcile=true` on `/sessions` forces a liveness sweep of every host before
answering. Do **not** use it for routine polling — a background reconciler
already runs every 30s, and forcing it puts an SSH round trip on every host
per request.

## Plans: confirm before you write

`POST /sessions/plan` and `POST /refresh/plan` run the same validation and
name-resolution as the real call (`fleet.resolveLaunch`, shared, so they cannot
disagree) and then do **read-only** checks on the host — `test -e`,
`git rev-parse`, `git status`, plus the cached preflight row. They create no
session row, write nothing to the host, and do not refresh the preflight cache.
There is a test asserting the commands sent are from an allowlist of reads.

A launch plan reports `worktree_exists` (will be reused), `dirty` (tracked
changes the agent starts on top of), `conflict` / `blocked` (an active session
for that task: the real call would 409), and `warnings`. `blocked: true` means
a confirmation prompt must not offer to proceed.

## Launching

```json
{ "host": "laptop-a", "path": "shop-agent", "task_id": "fix-tax",
  "prompt": "...", "branch": "master", "workspace": "worktree", "force": false }
```

`branch` is the **base** to cut from (defaults to the path's). `workspace` is
`worktree` (default) or `inplace`. `force` replaces a session that is already
running under that task id.

A conflict returns **409 with the existing session in the body**, so a caller
can use what is already running instead of retrying blind.

## Status codes

| Code | Meaning |
|---|---|
| 202 | launched; tmux is up, URL still being scraped |
| 400 | bad target — unknown `host`/`path`, or a jump-only host |
| 401 | wrong bearer token |
| 404 | no such session |
| 409 | task id already active, or the checkout is dirty, or a merge conflict |
| 424 | preflight failed — the host is missing linux/tmux/git/claude |

## Shape of a session

```json
{
  "id": "uuid", "task_id": "fix-tax", "runner": "claude-code",
  "host": "laptop-a", "repo_name": "shop-agent",
  "repo_dir": "/home/dev/src/shop-agent",
  "worktree": "/home/dev/src/agent-worktrees/shop-agent/fix-tax",
  "branch": "agent/fix-tax",
  "tmux_session": "agent-shop-agent-fix-tax",
  "log_path": "/home/dev/src/agent-logs/shop-agent/fix-tax.log",
  "remote_url": "https://claude.ai/code/session_...",
  "state": "running", "pane_pid": 12345, "exit_status": null,
  "prompt_preview": "first ~200 chars",
  "created_at": "...", "updated_at": "...", "last_seen_at": "...",
  "ended_at": null, "note": ""
}
```

`remote_url` may be empty — see `states.md`. `note` carries the reason a
session failed or stopped and is worth surfacing to the human.

## Refresh result

```json
{ "host": "laptop-b", "path": "shop-agent", "strategy": "ff-only",
  "before": "acef1043...", "after": "f5a01b00...", "changed": true,
  "diffstat": " 28 files changed...", "conflict": false, "conflicts": [],
  "was_dirty": false, "message": "" }
```

A conflict returns 409 with `conflict: true` and `conflicts` listing the
unmerged paths. The repo is deliberately **left mid-operation** for a human —
report the paths, do not abort.
