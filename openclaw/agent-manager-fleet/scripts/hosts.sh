#!/bin/sh
# Inventory: hosts, their allowed repos, and the last preflight result.
set -eu
. "$(dirname "$0")/_common.sh"

api GET /hosts | jq -r '
  (["HOST","ADDRESS","RUNNER","PATHS","PREFLIGHT"] | @tsv),
  (.hosts[]
   | [ .name, .address, .runner,
       ([.paths[]?.name] | join(",") | if . == "" then "-" else . end),
       (if .check == null then "unchecked"
        elif .check.ok then "ok"
        else "FAIL: " + (.check.note // "") end) ] | @tsv)' \
  | column -t -s "$(printf '\t')"

echo
api GET /hosts | jq -r '"ssh key: " + (.key_fingerprints // ["(none loaded)"] | join(", "))'
