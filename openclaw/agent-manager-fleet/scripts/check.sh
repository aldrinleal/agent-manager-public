#!/bin/sh
# Re-run preflight on one host: linux? tmux? git? claude?
set -eu
. "$(dirname "$0")/_common.sh"
[ $# -ge 1 ] || { echo "usage: check.sh <host>" >&2; exit 2; }

api POST "/hosts/$1/check" | jq -r '
  .check as $c
  | "host:    \($c.host)",
    "ok:      \($c.ok)",
    "os:      \($c.os // "-")",
    "tmux:    \($c.tmux_version // "-")",
    "git:     \($c.git_version // "-")",
    "runner:  \($c.runner_version // "-")",
    (if ($c.note // "") != "" then "note:    \($c.note)" else empty end)'
