#!/bin/sh
# Type an instruction into a running session and press Enter. WRITES.
#   send.sh <ref> <text>          # plan only
#   send.sh <ref> <text> --yes    # sends
set -eu
. "$(dirname "$0")/_common.sh"

YES=0; n=$#
while [ "$n" -gt 0 ]; do a=$1; shift; n=$((n-1)); [ "$a" = --yes ] && YES=1 || set -- "$@" "$a"; done

[ $# -ge 2 ] || { echo "usage: send.sh <session-id|task-id> <text> [--yes]" >&2; exit 2; }

id=$(resolve "$1"); shift
text=$*

if [ "$YES" -ne 1 ]; then
  api GET "/sessions/$id" | jq -r --arg t "$text" '
    .session as $s
    | "PLAN: type into a running agent",
      "",
      "  session:  \($s.task_id)  on \($s.host)/\($s.repo_name)  [\($s.state)]",
      "  it will be typed into the pane and Enter pressed, EXACTLY:",
      "    " + ($t | gsub("\n"; "\n    ")),
      (if ($s.state == "running" or $s.state == "starting" or $s.state == "stale") then empty
       else "", "  BLOCKED: the session is \($s.state); nothing is listening." end)'
  plan_footer "send.sh $id '<same text>'"
  exit 0
fi

api POST "/sessions/$id/send" "$(jq -n --arg t "$text" '{text:$t}')" >/dev/null
echo "sent"
