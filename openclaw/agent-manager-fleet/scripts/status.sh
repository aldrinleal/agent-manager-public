#!/bin/sh
# Git state of the allowlisted checkouts: which are dirty, ahead, behind, or
# stopped mid-merge/rebase. Read-only.
#
#   status.sh                  every host/path
#   status.sh laptop-a             one host
#   status.sh laptop-a/shop-agent    one path
#   status.sh --fresh ...      bypass the 60s cache
set -eu
. "$(dirname "$0")/_common.sh"

fresh=""; target=""
for a in "$@"; do
  case "$a" in --fresh) fresh="&fresh=true" ;; *) target=$a ;; esac
done

host=${target%%/*}; path=""
[ -n "$target" ] && [ "$target" != "$host" ] && path=${target#*/}

api GET "/paths?host=$host&path=$path$fresh" | jq -r '
  (["HOST","PATH","BRANCH","AHEAD/BEHIND","DIRTY","UNTRACKED","IN-PROGRESS"] | @tsv),
  (.paths[]
   | if .status == null then
       [.host, .path, "-", "-", "-", "-", "ERROR: " + (.error // "?")]
     else
       [.host, .path, (.status.branch // "(detached)"),
        (if .status.upstream == "" or .status.upstream == null then "no-upstream" else "+\(.status.ahead)/-\(.status.behind)" end),
        (.status.dirty | length | tostring), (.status.untracked | tostring),
        (if .status.in_progress then (.status.in_progress | ascii_upcase) else "-" end)]
     end | @tsv)' | column -t -s "$(printf '\t')"

echo
api GET "/paths?host=$host&path=$path" | jq -r '
  [.paths[] | select(.status != null) | select((.status.dirty | length) > 0 or .status.in_progress)]
  | if length == 0 then "all clean."
    else (.[] | "\(.host)/\(.path):" + (if .status.in_progress then "  [" + (.status.in_progress | ascii_upcase) + " STOPPED - needs a human]" else "" end),
          (.status.dirty[:8][] | "    M " + .),
          (if (.status.dirty | length) > 8 then "    ... and \((.status.dirty | length) - 8) more" else empty end)) end'
