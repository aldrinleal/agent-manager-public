#!/bin/sh
# Abandon a waiting `claude auth login` instead of letting it time out. WRITES,
# but only tears down the login session started for that purpose.
#   auth-cancel.sh <host>          # plan only
#   auth-cancel.sh <host> --yes
set -eu
. "$(dirname "$0")/_common.sh"

YES=0; n=$#
while [ "$n" -gt 0 ]; do a=$1; shift; n=$((n-1)); [ "$a" = --yes ] && YES=1 || set -- "$@" "$a"; done
[ $# -ge 1 ] || { echo "usage: auth-cancel.sh <host> [--yes]" >&2; exit 2; }

if [ "$YES" -ne 1 ]; then
  echo "PLAN: cancel the waiting claude login on $1"
  echo
  echo "  kills tmux session agentmgr-auth-$1; the existing login (if any) is untouched"
  plan_footer "auth-cancel.sh $1"
  exit 0
fi

api DELETE "/hosts/$1/auth/login" | jq -r 'if .cancelled then "cancelled" else "nothing was waiting" end'
