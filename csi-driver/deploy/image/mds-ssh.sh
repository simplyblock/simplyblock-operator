#!/usr/bin/env bash
#
# SSH into the pNFS metadata server's guest, installed as /usr/local/bin/ssh in
# the runner image so `kubectl exec -it <mds-pod> -- ssh` lands in the guest.
#
# The runner generates the key at every boot when started with -debug-ssh and
# hands the guest its public half; the private half never leaves this
# container. Arguments pass through to the SSH client, so
# `-- ssh cat /proc/meminfo` runs one command. The guest's address is the
# runner's fixed private one.

set -euo pipefail

key=/run/mds/ssh/id_ed25519
test -r "${key}" || {
    echo "No debug key at ${key}: the runner was not started with --debug-ssh." >&2
    exit 1
}
exec /usr/bin/ssh -i "${key}" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    root@169.254.100.2 "$@"
