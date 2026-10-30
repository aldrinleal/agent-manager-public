#!/bin/sh
# Every session in the fleet. Optional arg filters by state.
#   fleet.sh            all
#   fleet.sh running    only running
set -eu
. "$(dirname "$0")/_common.sh"

q=""
[ $# -gt 0 ] && q="?state=$1"

api GET "/sessions$q" | jq -r '
  if (.sessions // []) | length == 0 then
    "no sessions"
  else
    (["TASK","HOST","REPO","BRANCH","STATE","AGE","URL"] | @tsv),
    (.sessions[]
     | [ .task_id, .host, .repo_name, .branch,
         (.state + (if .exit_status != null then "(\(.exit_status))" else "" end)),
         (((now - ((.created_at|sub("\\.[0-9]+";"")|fromdateiso8601))) / 60) | floor | tostring + "m"),
         (.remote_url // "-") ] | @tsv)
  end' | column -t -s "$(printf '\t')"

api GET "/sessions?state=blocked" | jq -r '(.sessions // []) | if length == 0 then empty else "\nWAITING FOR YOU (\(length)):", (.[] | "  \(.task_id) on \(.host)/\(.repo_name): \(.note // "approval needed")\n    \(.remote_url // "(no url: use attach)")") end'
