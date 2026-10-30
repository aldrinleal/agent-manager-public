#!/bin/sh
# Git refresh a checkout. WRITES -- it can move a real working tree.
#   refresh.sh <host>[/<path>] [strategy] [--autostash]          # plan only
#   refresh.sh <host>[/<path>] [strategy] [--autostash] --yes    # refreshes
#
# Default strategy is ff-only, which cannot lose work. rebase/merge may stop
# on a conflict on purpose; report the conflicted paths, do not --abort.
#
# --autostash lets git set dirty tracked files aside around a rebase/merge and
# re-apply them. If re-applying conflicts, git keeps the stash and the result
# names it: report it, do not `stash drop`. Without it, a dirty checkout is
# refused. A checkout stopped mid-merge/rebase is always refused.
set -eu
. "$(dirname "$0")/_common.sh"

YES=0; AUTOSTASH=false; n=$#
while [ "$n" -gt 0 ]; do
  a=$1; shift; n=$((n-1))
  case "$a" in --yes) YES=1 ;; --autostash) AUTOSTASH=true ;; *) set -- "$@" "$a" ;; esac
done

[ $# -ge 1 ] || { echo "usage: refresh.sh <host>[/<path>] [strategy] [--yes]" >&2; exit 2; }

target=$1; strategy=${2:-}
host=${target%%/*}
path=""
[ "$target" != "$host" ] && path=${target#*/}

body=$(jq -n --arg h "$host" --arg p "$path" --arg s "$strategy" --argjson a "$AUTOSTASH" \
  '{host:$h} + (if $p != "" then {path:$p} else {} end) + (if $s != "" then {strategy:$s} else {} end) + (if $a then {autostash:true} else {} end)')
flag=""; [ "$AUTOSTASH" = true ] && flag=" --autostash"

if [ "$YES" -ne 1 ]; then
  api POST /refresh/plan "$body" | jq -r '
    "PLAN: git refresh",
    "",
    (.plan[] |
      "  \(.host)/\(.path)   \(.strategy)   from \(.remote)/\(.branch)   (HEAD \((.head // "?")[0:8]), +\(.ahead)/-\(.behind))",
      (if .in_progress then "    STOPPED MID-" + (.in_progress | ascii_upcase) + " -> would be REFUSED" else empty end),
      (if (.dirty // []) | length > 0 then "    tracked changes: " + ((.dirty | length | tostring)) + (if .will_stash then "  -> will be stashed and re-applied" elif .would_block then "  -> would be REFUSED" else "" end) else empty end),
      (.warnings[] | "    ! " + .))'
  blocked=$(api POST /refresh/plan "$body" | jq -r '[.plan[].would_block] | any')
  if [ "$blocked" = true ]; then echo; echo "At least one path would be refused as things stand."; fi
  plan_footer "refresh.sh $target $strategy$flag"
  exit 0
fi

res=$(api POST /refresh "$body")

# Only the table goes through `column`: a diffstat line is not a tab-separated
# row, and aligning it widens every column of the table above it.
printf '%s' "$res" | jq -r '
  ((.results // [.result]) // []) as $r
  | (["HOST","PATH","STRATEGY","BEFORE","AFTER","CHANGED"] | @tsv),
    ($r[] | select(. != null)
     | [ .host, .path, .strategy, (.before[0:8] // "-"), (.after[0:8] // "-"),
         (if .conflict then "CONFLICT" elif .changed then "yes" else "no" end) ] | @tsv)' \
  | column -t -s "$(printf '\t')"

printf '%s' "$res" | jq -r '
  ((.results // [.result]) // []) as $r
  | ($r[] | select(. != null) | select((.diffstat // "") != "")
     | "\n\(.host)/\(.path):\n\(.diffstat)"),
    ($r[] | select(. != null) | select((.stash_ref // "") != "")
     | "\n\(.host)/\(.path): your uncommitted changes are KEPT in stash \(.stash_ref[0:8]) - `git stash pop` once resolved (never dropped for you)"),
    ($r[] | select(. != null) | select((.conflicts // []) | length > 0)
     | "\n\(.host)/\(.path) CONFLICTS (resolve by hand, do not abort):\n  " + (.conflicts | join("\n  ")))'
