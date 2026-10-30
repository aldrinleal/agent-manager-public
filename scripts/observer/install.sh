#!/bin/sh
# Authorize agent-manager's SSH key on a host. Run by a PERSON, deliberately:
# this grants access to a machine, so it is not something the service or an
# assistant should do on its own.
#
#   install.sh --observer <user@host> <pubkey-file>   # restricted: two fixed commands only
#   install.sh --agent    <user@host> <pubkey-file>   # normal key, for hosts that run agents
#   add --dry-run to print what would be written without touching anything
#
# --observer is for servers and any machine that only needs "is it up" and "what
# is in its history.jsonl" (the k3s control plane, the workers). It copies
# agentmgr-observer.sh next to the account's home and authorizes the key as
#   restrict,command="$HOME/.agentmgr-observer.sh" ssh-ed25519 ...
# so the key can run `uname -s` and `cat ~/.claude/history.jsonl` and nothing
# else: no shell, no forwarding, no pty.
#
# --agent writes the same unrestricted line the other agent hosts carry.
#
# The public key is derived from the cluster Secret, without writing the private
# key anywhere:
#   kubectl -n default get secret agent-manager -o jsonpath='{.data.AGENTMGR_SSH_KEY}' \
#     | base64 -d | ssh-keygen -y -f /dev/stdin > agentmgr.pub
# Expected fingerprint (ssh-keygen -lf agentmgr.pub):
#   SHA256:<your key fingerprint>
#
# To REVOKE: delete the line containing "agent-manager" from ~/.ssh/authorized_keys
# on the host (and ~/.agentmgr-observer.sh for an observer).
set -eu

DRY=0; MODE=""; args=""
for a in "$@"; do
  case "$a" in
    --dry-run) DRY=1 ;;
    --observer|--agent) MODE=${a#--} ;;
    *) args="$args $a" ;;
  esac
done
set -- $args

[ -n "$MODE" ] && [ $# -eq 2 ] || { sed -n '2,24p' "$0" >&2; exit 2; }
target=$1; pubfile=$2
[ -r "$pubfile" ] || { echo "cannot read $pubfile" >&2; exit 2; }

key=$(awk '{print $1" "$2}' "$pubfile")
here=$(cd "$(dirname "$0")" && pwd)

case "$MODE" in
  observer) line="restrict,command=\"\$HOME/.agentmgr-observer.sh\" $key agent-manager observer (restricted)" ;;
  agent)    line="$key agent-manager@homelab (k8s service)" ;;
esac

if [ "$DRY" -eq 1 ]; then
  echo "would connect to: $target  (mode: $MODE)"
  [ "$MODE" = observer ] && echo "would copy:       $here/agentmgr-observer.sh -> ~/.agentmgr-observer.sh (0755)"
  echo "would append to ~/.ssh/authorized_keys:"
  echo "  $(printf '%s' "$line" | sed 's/AAAA[A-Za-z0-9+\/=]*/AAAA…/')"
  exit 0
fi

if [ "$MODE" = observer ]; then
  ssh "$target" 'cat > "$HOME/.agentmgr-observer.sh" && chmod 755 "$HOME/.agentmgr-observer.sh"' < "$here/agentmgr-observer.sh"
fi

# The marker makes it idempotent: a second run changes nothing.
marker=$(printf '%s' "$line" | awk '{print $NF}')
ssh "$target" "sh -s" <<REMOTE
mkdir -p "\$HOME/.ssh" && chmod 700 "\$HOME/.ssh"
touch "\$HOME/.ssh/authorized_keys" && chmod 600 "\$HOME/.ssh/authorized_keys"
if grep -q 'agent-manager' "\$HOME/.ssh/authorized_keys"; then
  echo "$target: agent-manager key already present; nothing changed"
else
  printf '%s\n' '$line' >> "\$HOME/.ssh/authorized_keys"
  echo "$target: authorized ($MODE)"
fi
REMOTE
