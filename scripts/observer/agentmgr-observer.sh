#!/bin/sh
# Forced command for agent-manager's key on an OBSERVER host.
#
# An observer (a server, or a machine with no tmux or claude) is reached with
# the same SSH key as every agent host, but there it must be able to do
# almost nothing: agent-manager is a remote-code-execution service by design,
# and a key that can run anything on the k3s control plane is not a price worth
# paying for "is it up" and "what is in its history.jsonl".
#
# Installed in authorized_keys as
#
#   restrict,command="/path/to/agentmgr-observer.sh" ssh-ed25519 AAAA... agent-manager
#
# `restrict` turns off port, agent and X11 forwarding and the pty; `command=`
# makes sshd run THIS regardless of what the client asked for, leaving the
# client's request in $SSH_ORIGINAL_COMMAND. Only these two exact requests are
# honoured. Anything else — including a shell, a pipe, or a variation on these —
# is refused. Keep this list in sync with fleet.checkObserver and
# fleet.PullHistory; a command added there must be added here, deliberately.
case "$SSH_ORIGINAL_COMMAND" in
  'uname -s')
    exec uname -s
    ;;
  'cat "$HOME/.claude/history.jsonl"')
    exec cat "$HOME/.claude/history.jsonl"
    ;;
  *)
    echo "agentmgr-observer: command not permitted for this key" >&2
    exit 126
    ;;
esac
