# Session states

```
starting ──▶ running ──▶ stopped
    │            │
    │            └──▶ stale ──▶ stopped
    └──▶ failed
```

| State | Means | Do |
|---|---|---|
| `starting` | tmux is up; the Remote Control URL has not appeared yet | Wait. Up to `AGENTMGR_URL_SCRAPE_TIMEOUT_S` (default 120s). |
| `running` | the pane is alive | Normal. |
| `blocked` | alive and **waiting for a human**: a permission or folder-trust prompt is on screen. The `note` carries the question (`waiting for you: Do you want to create x.txt?`) | **Tell the human, with the question.** See the rule below. |
| `stale` | alive, but the log has not grown for `AGENTMGR_STALE_AFTER_S` (default 30 min) | **Read the log before concluding anything.** |
| `stopped` | the pane exited or the session is gone; `exit_status` is set | Check `exit_status` and `note`. |
| `failed` | the launch itself broke — git, tmux or preflight | `note` says which step. Nothing is running. |

## `blocked` — waiting for you

`blocked` is the state a fleet view most needs: the agent stopped because it asked
for approval and nobody has answered. It is decided from the **visible screen** of
the session (not its log), so it clears the moment the prompt is answered.
It outranks `starting` (a session stopped at the folder-trust prompt never
prints a URL) and `stale` (a quiet log is exactly what a blocked session looks like).

**Never approve it on the human's behalf.** `send.sh` can type "1" and Enter, and
that would approve a tool call on a remote machine. That decision belongs to the
person: show them the note, and the URL or `attach` commands so they can look at the
screen and answer it themselves. Auto-approving is the opposite of this
skill's confirm-everything rule.

## Three things that mislead

**1. A missing `remote_url` does not mean failure.** The URL is scraped from
the agent's own terminal output. If the scrape times out, or Remote Control
was never enabled, the field stays empty while the session runs perfectly
well. Liveness is established separately, from `tmux`. Use
`GET /sessions/{id}/attach` for the `ssh` + `tmux attach` route instead.

**2. `stale` is not "dead" or even "stuck".** It is purely "no new output for
a while". An agent thinking through a hard problem, or one sitting at a prompt
waiting for the human, produces no output and looks identical to one that has
wedged. Read the log.

**3. `stopped` with `exit_status: 0` is success.** The agent finished. It is
not an error, and the worktree still holds whatever it did.

## How liveness is determined

A background sweep runs every `AGENTMGR_RECONCILE_INTERVAL_S` (default 30s):
one `tmux list-panes -a` per host plus one `stat` over the log files — one
round trip per host, not per session.

- pane alive + log growing → `running`
- pane alive + log quiet → `stale`
- pane dead (`remain-on-exit` keeps it visible) → `stopped`, with a real exit
  status
- session gone entirely → `stopped`, note says so

This is why the state is trustworthy even when the URL is not: it comes from
tmux on the host, not from anything the vendor reports.
