#!/bin/sh
# The Remote Control URL for a session. Empty URL is not an error -- the
# session is still reachable; this prints how.
set -eu
. "$(dirname "$0")/_common.sh"
[ $# -ge 1 ] || { echo "usage: url.sh <session-id|task-id>" >&2; exit 2; }

id=$(resolve "$1")
url=$(api GET "/sessions/$id" | jq -r '.session.remote_url // ""')

if [ -n "$url" ]; then
  echo "$url"
else
  echo "no Remote Control URL for this session (it may still be starting)." >&2
  echo "It is still reachable directly:" >&2
  api GET "/sessions/$id/attach" | jq -r '.attach | (if (.exec_command // "") != "" then "  \(.exec_command)" else "  \(.ssh_command)\n  \(.tmux_command)" end) + "\n  log: \(.log_path)"'
fi
