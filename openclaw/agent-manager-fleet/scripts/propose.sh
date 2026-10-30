#!/bin/sh
# Which repositories do you actually work in that are not in the allowlist yet?
# Read-only. Ranked by DISTINCT SESSIONS, not prompts: one marathon session can
# hold thousands of prompts and must not outrank a repo returned to every day.
#
#   propose.sh           last 90 days
#   propose.sh 30        last 30 days
#   propose.sh --all     include the skipped ones with their reasons (default shows only
#                        proposals, allowlisted and near-misses worth a look)
#
# It never changes the allowlist: that file is the security boundary. Paste the
# YAML it prints under the host's `paths:` in hosts.yaml, then `agentmgr inventory render`
# and redeploy.
set -eu
. "$(dirname "$0")/_common.sh"

days=90; all=0
for a in "$@"; do case "$a" in --all) all=1 ;; [0-9]*) days=$a ;; esac; done

res=$(api GET "/history/proposals?days=$days")

printf '%s' "$res" | jq -r --argjson all "$all" '
  (["#","PROJECT","SESS","AGE","ON HOSTS","VERDICT"] | @tsv),
  ([.proposals[] | select($all == 1 or .verdict != "skip" or (.reason | test("dormant|empty|subdirectory|not a git")))]
   | to_entries[]
   | .key as $i | .value as $p
   | [($i+1), ($p.project | sub("^/home/[^/]+/src/"; "")), $p.sessions, "\($p.age_days)d",
      (if ($p.checkouts | length) > 0 then ($p.checkouts | join(",")) else "-" end),
      (if $p.verdict == "propose" then "PROPOSE" elif $p.verdict == "allowlisted" then "allowlisted" else "skip - " + $p.reason end)] | @tsv)' \
  | column -t -s "$(printf '\t')"

unreach=$(printf '%s' "$res" | jq -r '(.unreachable // []) | join(" ")')
[ -z "$unreach" ] || { echo; echo "! no answer from: $unreach - their checkouts are unknown, so 'not checked out' may just mean asleep"; }

yaml=$(printf '%s' "$res" | jq -r '.yaml')
if [ -n "$(printf '%s' "$yaml" | tr -d '[:space:]')" ]; then
  echo; echo "paste under the host's paths: in hosts.yaml:"; echo; printf '%s\n' "$yaml" | sed 's/^/  /'
else
  echo; echo "nothing to propose."
fi
