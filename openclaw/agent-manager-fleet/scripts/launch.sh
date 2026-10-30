#!/bin/sh
# Start an agent session. WRITES -- two phases, see _common.sh.
#
#   launch.sh <host>/<path> <task-id> <prompt>          # prints the plan, does nothing
#   launch.sh <host>/<path> <task-id> <prompt> --yes    # launches
#
# Exit codes: 0 plan shown / launched, 3 plan shows the launch would be refused.
# Pass the prompt as ONE quoted argument: a bare --yes among its words would be
# taken as the confirmation flag.
set -eu
. "$(dirname "$0")/_common.sh"

YES=0; n=$#
while [ "$n" -gt 0 ]; do a=$1; shift; n=$((n-1)); [ "$a" = --yes ] && YES=1 || set -- "$@" "$a"; done

[ $# -ge 3 ] || { echo "usage: launch.sh <host>/<path> <task-id> <prompt> [--yes]" >&2; exit 2; }

target=$1; task=$2; shift 2; prompt=$*

host=${target%%/*}
path=${target#*/}
[ "$host" != "$path" ] || { echo "target must be <host>/<path>; see hosts.sh" >&2; exit 2; }

body=$(jq -n --arg h "$host" --arg p "$path" --arg t "$task" --arg pr "$prompt" \
  '{host:$h, path:$p, task_id:$t, prompt:$pr}')

if [ "$YES" -ne 1 ]; then
  plan=$(api POST /sessions/plan "$body")

  printf '%s' "$plan" | jq -r '
    .plan as $p
    | "PLAN: launch an agent",
      "",
      "  host:       \($p.host)",
      "  repo:       \($p.path)   (\($p.repo_dir))",
      "  task:       \($p.task_id)",
      "  workspace:  \($p.workspace)" + (if $p.workspace == "worktree" then "  -> NEW worktree, branch \($p.branch), cut from \($p.base)" else "  -> runs IN the checkout, branch \($p.branch)" end),
      "  worktree:   \($p.worktree)" + (if $p.worktree_exists then "   (ALREADY EXISTS - will be reused)" else "" end),
      "  tmux:       \($p.tmux_session)",
      "  log:        \($p.log_path)",
      "  preflight:  " + (if $p.preflight == null then "not run yet" elif $p.preflight.ok then "ok (\($p.preflight.runner_version))" else "FAILED" end),
      "",
      "  prompt:",
      "    " + ($p.prompt_preview | gsub("\n"; "\n    ")),
      (if ($p.warnings | length) > 0 then "", "  WARNINGS:", ($p.warnings[] | "    ! " + .) else empty end),
      (if $p.conflict != null then "", "  BLOCKED: task \($p.conflict.task_id) is already \($p.conflict.state) on this host/repo (session \($p.conflict.id)). Use another task id, or kill that one first." else empty end)'

  if [ "$(printf '%s' "$plan" | jq -r '.plan.blocked')" = true ]; then
    echo; echo "Cannot proceed as planned."; exit 3
  fi

  plan_footer "launch.sh $target $task '<same prompt>'"
  exit 0
fi

api POST /sessions "$body" | jq -r '.session | "launched \(.task_id) on \(.host)/\(.repo_name)\n  branch:   \(.branch)\n  worktree: \(.worktree)\n  state:    \(.state)"'

echo "waiting for the Remote Control URL..."
i=0
while [ $i -lt 30 ]; do
  url=$(api GET "/sessions?task_id=$task" | jq -r '.sessions[0].remote_url // ""')
  [ -n "$url" ] && { echo "$url"; exit 0; }
  i=$((i+1)); sleep 4
done

echo "no URL yet after ~2 min. The session is running regardless -- see log.sh / url.sh." >&2
