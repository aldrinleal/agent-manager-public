#!/bin/sh
# Shared helpers. Sourced by every script here; not runnable on its own.

: "${AGENTMGR_URL:=http://localhost:8000}"

API="${AGENTMGR_URL%/}/api/fleet/v1"

if [ -z "$AGENTMGR_API_TOKEN" ]; then
  echo "AGENTMGR_API_TOKEN is not set." >&2
  echo "It is the AGENTMGR_API_TOKEN key of the 'agent-manager' Kubernetes Secret:" >&2
  echo "  kubectl get secret agent-manager -o jsonpath='{.data.AGENTMGR_API_TOKEN}' | base64 -d" >&2
  exit 2
fi

command -v jq >/dev/null 2>&1 || { echo "jq is required." >&2; exit 2; }

# api <method> <path> [json-body]
#
# Fails loudly on a non-2xx rather than letting jq parse an error body into
# empty output — a silent empty table is the worst possible answer here.
api() {
  _method=$1; _path=$2; _body=${3:-}

  if [ -n "$_body" ]; then
    _out=$(curl -sS -m 120 -w '\n%{http_code}' -X "$_method" \
      -H "Authorization: Bearer $AGENTMGR_API_TOKEN" \
      -H 'Content-Type: application/json' \
      -d "$_body" "$API$_path")
  else
    _out=$(curl -sS -m 120 -w '\n%{http_code}' -X "$_method" \
      -H "Authorization: Bearer $AGENTMGR_API_TOKEN" "$API$_path")
  fi

  _code=$(printf '%s' "$_out" | tail -n1)
  _json=$(printf '%s' "$_out" | sed '$d')

  case "$_code" in
    2*) printf '%s' "$_json" ;;
    401) echo "401 unauthorized — AGENTMGR_API_TOKEN is wrong." >&2; exit 1 ;;
    000) echo "could not reach $AGENTMGR_URL (is agentmgrd running? set AGENTMGR_URL)" >&2; exit 1 ;;
    *)   echo "HTTP $_code: $(printf '%s' "$_json" | jq -r '.error // .' 2>/dev/null)" >&2; exit 1 ;;
  esac
}

# Resolve a task id to a session id; pass a session id through untouched.
resolve() {
  _ref=$1
  [ -n "$_ref" ] || { echo "usage: need a session id or task id" >&2; exit 2; }

  _id=$(api GET "/sessions?task_id=$_ref" | jq -r '
    (.sessions // []) as $s
    | ([$s[] | select(.state=="running" or .state=="starting" or .state=="stale")]) as $live
    | if   ($live|length) == 1 then $live[0].id
      elif ($s|length)    == 1 then $s[0].id
      else "" end')

  if [ -n "$_id" ] && [ "$_id" != "null" ]; then printf '%s' "$_id"; return; fi

  # Not a task id (or ambiguous) — assume it is already a session id.
  printf '%s' "$_ref"
}

# --- confirm-before-write ----------------------------------------------------
#
# Every script that WRITES (launch, send, kill, refresh, push) runs in two
# phases. Without --yes it prints a plan and changes nothing; with --yes it
# executes. The flag is the guarantee: an agent cannot perform a write by
# accident, only by passing --yes after showing the plan to the human.
#
# Each script strips --yes itself (a function cannot rewrite its caller's
# "$@"), using:
#
#   YES=0; n=$#
#   while [ "$n" -gt 0 ]; do a=$1; shift; n=$((n-1)); [ "$a" = --yes ] && YES=1 || set -- "$@" "$a"; done

# plan_footer <command-that-would-run>
plan_footer() {
  echo
  echo "NOT DONE. This was a plan: nothing was changed."
  echo "Show it to the human. Only if they say yes to THIS plan, re-run the same"
  echo "command with --yes appended:"
  echo "  $1 --yes"
}
