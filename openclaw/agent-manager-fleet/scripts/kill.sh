#!/bin/sh
# Stop a session. WRITES.
#   kill.sh <ref> [--remove-worktree]          # plan only
#   kill.sh <ref> [--remove-worktree] --yes    # kills
#
# The worktree is KEPT unless --remove-worktree is passed -- it holds whatever
# the agent did.
set -eu
. "$(dirname "$0")/_common.sh"

YES=0; n=$#
while [ "$n" -gt 0 ]; do a=$1; shift; n=$((n-1)); [ "$a" = --yes ] && YES=1 || set -- "$@" "$a"; done

[ $# -ge 1 ] || { echo "usage: kill.sh <session-id|task-id> [--remove-worktree] [--yes]" >&2; exit 2; }

id=$(resolve "$1")
rm=false
[ "${2:-}" = "--remove-worktree" ] && rm=true

if [ "$YES" -ne 1 ]; then
  api GET "/sessions/$id" | jq -r --arg rm "$rm" '
    .session as $s
    | "PLAN: stop an agent",
      "",
      "  session:   \($s.task_id)  on \($s.host)/\($s.repo_name)  [\($s.state)]",
      "  tmux:      \($s.tmux_session)  (will be killed)",
      "  worktree:  \($s.worktree)",
      (if $rm == "true"
       then "  -> the worktree AND everything uncommitted in it will be DELETED. Not recoverable.",
            "     Branch \($s.branch) is kept, but unpushed or uncommitted work is lost."
       else "  -> the worktree is KEPT." end)'
  plan_footer "kill.sh $id$( [ "$rm" = true ] && echo ' --remove-worktree' )"
  exit 0
fi

api DELETE "/sessions/$id?remove_worktree=$rm" \
  | jq -r --arg rm "$rm" '.session | "killed \(.tmux_session) on \(.host)" + (if $rm == "true" then " (worktree removed)" else " (worktree \(.worktree) kept)" end)'
