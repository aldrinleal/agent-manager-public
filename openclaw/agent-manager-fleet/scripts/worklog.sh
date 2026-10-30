#!/bin/sh
# What did I work on? By day, then project, then session, from the collected
# ~/.claude/history.jsonl. Read-only.
#
#   worklog.sh                 last 7 days, all hosts
#   worklog.sh 60              last 60 days
#   worklog.sh 14 laptop-a         one host
#   worklog.sh --counts 60     only per-day/project counts, no prompt text
#
# Day boundaries use $TZ (or the machine's zone), so 23:30 local is that day.
# Prompt text appears only if the service stores it (AGENTMGR_HISTORY_STORE_PROMPTS).
set -eu
. "$(dirname "$0")/_common.sh"

days=7; host=""; counts=0
for a in "$@"; do
  case "$a" in --counts) counts=1 ;; [0-9]*) days=$a ;; *) host=$a ;; esac
done

tz=${TZ:-}
[ -n "$tz" ] || tz=$(timedatectl show -p Timezone --value 2>/dev/null || cat /etc/timezone 2>/dev/null || echo UTC)

api GET "/history/worklog?days=$days&host=$host&tz=$tz" | jq -r --argjson counts "$counts" '
  if (.days | length) == 0 then "no history in this window (has `history-pull.sh` run?)" else
  .days[] |
    "\n== \(.date) ==",
    (.projects[] |
      "  \(.project | sub("^/home/[^/]+/src/"; ""))  (\(.prompt_count) prompts, \(.sessions | length) session(s))",
      (if $counts == 1 then empty else
        (.sessions[] | .host as $h
         | (.prompts[:6][] | "      [\($h)] " + (gsub("\n"; " ") | .[0:140])),
           (if (.prompts | length) > 6 then "      ... +\((.prompts | length) - 6) more" else empty end)) end))
  end'
