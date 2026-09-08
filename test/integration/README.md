<!-- What running this suite needs from the host, and what to do when a run
leaves the host unable to run it again. The Makefile carries the same facts as
one-line comments beside the targets; this is the long form, because two of
these things cost a day to work out and neither is discoverable from a failure
message. -->

# Integration suite

Boots a Talos cluster in QEMU and runs the tests against it. The cases here need
a real kernel: nodes must host an NVMe-oF target, assemble LVM, and mount a
filesystem, so nothing here runs against a fake.

## What the host has to have

| Requirement                       | Why                                                        |
|-----------------------------------|------------------------------------------------------------|
| `talosctl`, `qemu-img`, `kubectl` | The provisioner and the cluster it builds                  |
| Linux: `/dev/kvm`                 | Without it QEMU emulates, which is far too slow to be used |
| macOS: ARM64                      | Talos's QEMU provisioner supports no other Mac             |
| root, for `talosctl` only         | The guest network: a CNI bridge on Linux, vmnet on macOS   |

`make preflight` checks all of it and reports what is missing.

## The sudo grants

The harness elevates `talosctl` rather than the test process, so a test writes
its artifacts as the developer rather than as root. That leaves a handful of
commands that genuinely need root, and a run in a terminal simply prompts for
each. A run with no terminal, in CI or under a tool, cannot answer a prompt: the
command fails, and what surfaces is a port conflict or a missing interface
rather than a missing credential.

So a host that runs this unattended needs these, and nothing wider:

```
<user> ALL=(root) SETENV: NOPASSWD: /opt/homebrew/bin/talosctl
<user> ALL=(root) NOPASSWD: /usr/sbin/chown -R <uid>\:<gid> /var/folders/*
<user> ALL=(root) NOPASSWD: /usr/sbin/chown -R <uid>\:<gid> /Users/<user>/.talos/clusters/*
<user> ALL=(root) NOPASSWD: /bin/rm -rf /Users/<user>/.talos/clusters/*
<user> ALL=(root) NOPASSWD: /usr/bin/pkill -x bootpd
```

`talosctl` needs `SETENV` because the harness passes `-E`: talosctl reads `HOME`
for its image cache. The two `chown` rules hand back what talosctl wrote as
root, one for the run's work directory and one for the QEMU monitor sockets the
defect cases connect to. The `rm` rule clears a state directory talosctl left
behind. `pkill -x bootpd` is the macOS case below, and `-x` matches that one
process name and nothing else.

## macOS: the fight over port 67

A `cluster create` on macOS can fail with either of these, and both are the same
thing:

```
error creating dhcpd: failure: DHCPd server has not started
<state>/dhcpd.log: error on dhcp4 startup: cannot bind to port 67: address already in use
```

Talos runs its own DHCP server for the cluster network, on port 67, and macOS
wants that port for `bootpd`. talosctl unloads `bootpd` before it starts, which
would settle it, except for the order: the network and **the nodes** are created
first, a node attaches to the network with `vmnet-shared`, and macOS enables and
loads `bootpd` again to serve DHCP on that vmnet. Talos's own server is started
after the nodes, and whichever of the two binds first keeps the port.

Measured on one host: the job goes from `disabled` to `enabled` in the same
second the bind fails, with no `bootpd` process alive beforehand. Whether a run
wins that race is not something this suite can decide, and `launchctl unload`
cannot help once a `bootpd` is already running, which is why the grant above is
a kill.

A run that loses it leaves the host unable to win the next one, so sweep before
retrying:

```
make clean-clusters
```

## Recovering from a create that died early

talosctl writes `state.yaml` once a cluster is up. A create that failed before
that point cannot be torn down by `talosctl cluster destroy`, which reports
`failed to read cluster state` and leaves the QEMU processes running as root,
holding the vmnet interface. `make clean-clusters` sweeps them, and a teardown
that could not finish names the processes and the one command that ends them.
