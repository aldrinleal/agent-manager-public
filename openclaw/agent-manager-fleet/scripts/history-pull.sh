#!/bin/sh
# Read ~/.claude/history.jsonl from every host (or one) into the service now,
# instead of waiting for the twice-a-day collector. WRITES to the service's own
# registry only; idempotent (a second run inserts 0). A host that cannot be
# reached is noted, never fatal.
#   history-pull.sh [host]          # plan only
#   history-pull.sh [host] --yes
set -eu
. "$(dirname "$0")/_common.sh"

YES=0; host=""
for a in "$@"; do case "$a" in --yes) YES=1 ;; *) host=$a ;; esac; done

if [ "$YES" -ne 1 ]; then
  echo "PLAN: read the Claude Code history from ${host:-every host}"
  echo
  echo "  runs one fixed read-only command per host: cat \"\$HOME/.claude/history.jsonl\""
  echo "  stores new prompts in the service's registry; nothing on the hosts changes"
  plan_footer "history-pull.sh $host"
  exit 0
fi

api POST /history/pull "$(jq -n --arg h "$host" 'if $h == "" then {} else {host:$h} end')" | jq -r '
  (["HOST","BYTES","PARSED","NEW","NOTE"] | @tsv),
  (.pulls[] | [.host, .bytes, .parsed, .inserted, (.note // "" | .[0:70])] | @tsv)' | column -t -s "$(printf '\t')"
