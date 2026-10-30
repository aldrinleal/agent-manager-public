#!/bin/sh
# Give a waiting `claude auth login` the code the browser showed. WRITES.
#   auth-code.sh <host> <code>          # plan only
#   auth-code.sh <host> <code> --yes    # types the code in and reports the outcome
#
# Success means the login process FINISHED and `claude auth status` is logged
# in -- not merely that status reads "logged in", which is also true before the
# code on an already-logged-in host.
set -eu
. "$(dirname "$0")/_common.sh"

YES=0; n=$#
while [ "$n" -gt 0 ]; do a=$1; shift; n=$((n-1)); [ "$a" = --yes ] && YES=1 || set -- "$@" "$a"; done

[ $# -ge 2 ] || { echo "usage: auth-code.sh <host> <code> [--yes]" >&2; exit 2; }
host=$1; code=$2

if [ "$YES" -ne 1 ]; then
  echo "PLAN: type a login code into the waiting claude login on $host"
  echo
  echo "  it is typed into tmux session agentmgr-auth-$host and Enter pressed"
  echo "  then the login's result is checked and, on success, preflight is re-run"
  echo "  (only letters, digits and - _ # . ~ + / = % : are accepted as a code)"
  plan_footer "auth-code.sh $host '<the same code>'"
  exit 0
fi

api POST "/hosts/$host/auth/code" "$(jq -n --arg c "$code" '{code:$c}')" | jq -r '
  .result as $r
  | if $r.logged_in then "logged in on \($r.host) as \($r.email // "?")"
    else "NOT logged in on \($r.host)",
         (if $r.retry then "the login is still waiting: check the code and run auth-code.sh again" else "the login process ended: start a new one with auth-login.sh" end),
         "", "terminal showed:", ($r.screen | split("\n") | map("  " + .) | join("\n")) end'
