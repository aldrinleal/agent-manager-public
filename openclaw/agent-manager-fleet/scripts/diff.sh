#!/bin/sh
# What did the agent do? Worktree state, commits ahead of the base, and a
# diffstat against the fork point. Read-only. Run this before pushing anything.
#   diff.sh <session-id|task-id>
set -eu
. "$(dirname "$0")/_common.sh"
[ $# -ge 1 ] || { echo "usage: diff.sh <session-id|task-id>" >&2; exit 2; }

id=$(resolve "$1")
api GET "/sessions/$id/diff" | jq -r '
  .diff as $d
  | "session:  \($d.task_id)",
    "worktree: \($d.worktree)",
    "branch:   \($d.status.branch // "(detached)")   base: \($d.base // "?")   ahead \($d.status.ahead) / behind \($d.status.behind)",
    (if $d.status.in_progress then "STOPPED MID-" + ($d.status.in_progress | ascii_upcase) + " - needs a human" else empty end),
    "",
    "commits not on the base (\($d.commits | length)):",
    (if ($d.commits | length) == 0 then "  (none)" else ($d.commits[] | "  " + .) end),
    "",
    "uncommitted tracked changes (\($d.status.dirty | length)):",
    (if ($d.status.dirty | length) == 0 then "  (none)" else ($d.status.dirty[] | "  M " + .) end),
    (if $d.status.untracked > 0 then "  + \($d.status.untracked) untracked file(s), not shown" else empty end),
    "",
    "diffstat vs fork point:",
    (if ($d.diffstat // "") == "" then "  (empty)" else ($d.diffstat | split("\n") | map("  " + .) | join("\n")) end),
    (if ($d.warnings | length) > 0 then "", ($d.warnings[] | "! " + .) else empty end)'
