#!/bin/sh
# Is `claude` logged in on each host? Read-only; reads the last preflight.
#   auth.sh            every host
#   auth.sh --fresh    re-run preflight first (one SSH round trip per host)
set -eu
. "$(dirname "$0")/_common.sh"

if [ "${1:-}" = "--fresh" ]; then
  for h in $(api GET /hosts | jq -r '.hosts[] | select((.role // "") == "") | .name'); do
    api POST "/hosts/$h/check" >/dev/null || true
  done
fi

api GET /hosts | jq -r '
  (["HOST","CLAUDE LOGIN","ACCOUNT","PLAN","CHECKED"] | @tsv),
  (.hosts[] | select((.role // "") == "")
   | .check as $c
   | [ .name,
       (if $c == null then "unchecked"
        elif $c.auth_logged_in == true then "logged in"
        elif $c.auth_logged_in == false then "LOGGED OUT"
        else "unknown" end),
       ($c.auth_email // "-"), ($c.auth_subscription // "-"),
       ($c.checked_at // "-" | .[0:19]) ] | @tsv)' | column -t -s "$(printf '\t')"

out=$(api GET /hosts | jq -r '[.hosts[] | select(.check != null and .check.auth_logged_in == false) | .name] | join(" ")')
[ -z "$out" ] || { echo; echo "Logged out: $out"; echo "Fix with: auth-login.sh <host>   (then auth-code.sh <host> <code>)"; }
