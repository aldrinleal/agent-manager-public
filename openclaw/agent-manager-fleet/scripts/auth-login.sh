#!/bin/sh
# Start a `claude auth login` on a host and get the link to authorize it. WRITES.
#   auth-login.sh <host>          # plan only
#   auth-login.sh <host> --yes    # starts it and prints the login URL
#
# The URL opens on ANY device (a phone is fine). After authorizing, the browser
# shows a code; give it to auth-code.sh. Run on a terminal, this script will ask
# for the code itself and submit it.
#
# Treat the URL and the code as secrets: do not paste them into logs or chats
# beyond the person who needs them.
set -eu
. "$(dirname "$0")/_common.sh"

YES=0; n=$#
while [ "$n" -gt 0 ]; do a=$1; shift; n=$((n-1)); [ "$a" = --yes ] && YES=1 || set -- "$@" "$a"; done

[ $# -ge 1 ] || { echo "usage: auth-login.sh <host> [--yes]" >&2; exit 2; }
host=$1

if [ "$YES" -ne 1 ]; then
  api GET "/hosts/$host/auth" | jq -r --arg h "$host" '
    .auth as $a
    | "PLAN: start a claude login on \($h)",
      "",
      "  a tmux session `agentmgr-auth-\($h)` will run `claude auth login`",
      "  it prints a login URL; you open it (any device), authorize, and receive a code",
      "  the code is then typed into that session; the session is killed afterwards",
      "  an unused login session is killed after the configured timeout (default 10 min)",
      "",
      "  current state: " + (if $a == null then "never checked"
                             elif $a.auth_logged_in == true then "LOGGED IN as \($a.auth_email // "?") (\($a.auth_subscription // "?"))"
                             elif $a.auth_logged_in == false then "logged out"
                             else "unknown" end),
      (if $a != null and $a.auth_logged_in == true then "", "  ! this host is already logged in; a successful login REPLACES that one." else empty end)'
  plan_footer "auth-login.sh $host"
  exit 0
fi

login=$(api POST "/hosts/$host/auth/login")

printf '%s' "$login" | jq -r '
  .login as $l
  | (if $l.reused then "a login was already waiting on this host; reusing it" else "login started" end),
    (if ($l.warnings | length) > 0 then ($l.warnings[] | "! " + .) else empty end),
    "",
    "Open this URL, authorize, and copy the code it shows:",
    "",
    "  " + $l.url,
    "",
    "(waits \($l.expires_in)s for the code)"'

if [ -t 0 ]; then
  printf '\nPaste the code here (or Ctrl-C and use auth-code.sh later): '
  read -r code
  [ -n "$code" ] && exec "$(dirname "$0")/auth-code.sh" "$host" "$code" --yes
else
  echo
  echo "Then:  auth-code.sh $host <code> --yes"
fi
