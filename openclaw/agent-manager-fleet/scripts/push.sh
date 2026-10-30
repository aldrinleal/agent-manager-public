#!/bin/sh
# Push a session's agent/<task> branch so a pull request can be opened. WRITES.
#   push.sh <ref>          # plan only: commits that would travel, and what would not
#   push.sh <ref> --yes    # pushes, then prints the pull-request link
#
# There is no force option and there never will be. A rejected push comes back
# as an error with the remote's message; what to do about it is the human's call.
# Only the branch the session created is ever pushed, from its worktree.
#
# Exit codes: 0 plan shown / pushed, 3 the plan shows the push would be refused.
set -eu
. "$(dirname "$0")/_common.sh"

YES=0; n=$#
while [ "$n" -gt 0 ]; do a=$1; shift; n=$((n-1)); [ "$a" = --yes ] && YES=1 || set -- "$@" "$a"; done

[ $# -ge 1 ] || { echo "usage: push.sh <session-id|task-id> [--yes]" >&2; exit 2; }

id=$(resolve "$1")

if [ "$YES" -ne 1 ]; then
  plan=$(api POST "/sessions/$id/push/plan")

  printf '%s' "$plan" | jq -r '
    .plan as $p
    | "PLAN: push an agent branch",
      "",
      "  session:   \($p.task_id)  on \($p.host)",
      "  worktree:  \($p.worktree)",
      "  push:      \($p.branch)  ->  \($p.remote)   (sets upstream)",
      "  never forced: if the remote has moved, the push is rejected and nothing changes",
      "",
      "  commits that would be sent (\($p.commits | length)):",
      (if ($p.commits | length) == 0 then "    (none)" else ($p.commits[] | "    " + .) end),
      (if ($p.uncommitted | length) > 0
       then "", "  NOT sent - uncommitted tracked changes (\($p.uncommitted | length)):", ($p.uncommitted[:8][] | "    M " + .)
       else empty end),
      (if ($p.warnings | length) > 0 then "", "  WARNINGS:", ($p.warnings[] | "    ! " + .) else empty end),
      (if $p.blocked then "", "  BLOCKED:", ($p.reasons[] | "    - " + .) else empty end)'

  if [ "$(printf '%s' "$plan" | jq -r '.plan.blocked')" = true ]; then
    echo; echo "Cannot proceed as planned."; exit 3
  fi

  plan_footer "push.sh $id"
  exit 0
fi

api POST "/sessions/$id/push" | jq -r '
  .push as $r
  | "pushed \($r.branch) to \($r.remote)",
    (if ($r.pr_url // "") != "" then "", "open a pull request:", "  " + $r.pr_url else "" end),
    "",
    "remote said:",
    ($r.output | split("\n") | map("  " + .) | join("\n"))'
