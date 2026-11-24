# Idea: one device-event source instead of several pollers

Status: idea, not a design. Captured so it isn't lost. Not scheduled.

## Where this came from

Investigating issue 10 (pNFS clients failing to open their block layout's
device) found the root cause in `ensureDeviceAlias`
(`internal/csi/node/pnfs.go`): the `/dev/disk/by-id/nvme-eui.<nguid>` alias
the kernel resolves a block layout through is created once, at
`NodeStageVolume`, and nothing re-creates it when the local device node for
an already-staged volume's namespace changes. Two other places in the driver
already detect that kind of change and neither one called it:

- `internal/reconnect` repairs degraded NVMe-oF paths and can bring a volume
  back on a different `/dev/nvmeN`.
- `healStack` (`internal/csi/node/stage.go`) re-observes the device after
  healing an unhealthy stack and updates its own bookkeeping, not the alias.

The planned fix is a reconciliation loop: re-derive each staged pNFS volume's
current `(nguid, devicePath)` from sysfs on a timer and re-run
`ensureDeviceAlias` unconditionally, the same shape `internal/initiator`
(`presence.go`) already uses for a different question (did a device vanish
entirely). That fix is enough to close the bug. It is not enough to close
the underlying pattern.

## What's duplicated

Three independent places in this driver each maintain their own answer to
"has something about this node's devices changed":

- `internal/initiator/presence.go` — a map of `devicePath -> lvolID`, written
  on attach, diffed against a live sysfs scan to catch a device that
  disappeared.
- `internal/reconnect` — `maxSeenPathsMap`, polling NVMe-oF subsystem health
  to decide a subsystem is short of its published redundancy.
- The planned alias reconciler — re-reading `(nguid, devicePath)` per staged
  pNFS volume to decide the by-id alias is stale.

Each one scans its own slice of system state, on its own schedule, with its
own bespoke diffing logic, to answer a variant of the same question. None of
them shares a detection mechanism with the others; each was written as if it
were the only thing that needed to know when a device changed.

## Why that's a problem

It isn't just repeated code. It's a correctness property — "this driver's
record of a device's identity and location is current" — with no single
owner. The bug this grew out of is a direct instance: the alias is a fourth
thing that needed the same kind of update, and because there was no shared
place that already fans a "device changed" signal out to every interested
party, the two places best positioned to deliver it (`internal/reconnect`'s
repair, `healStack`'s heal) simply didn't know they needed to, and nobody
noticed until a layout started silently routing through the metadata server
for ten days.

A fourth bespoke poller closes today's instance of the gap. It doesn't close
the gap itself: the next piece of logic that needs to react to a device
change is exactly as likely to be wired up correctly as the alias was,
because "wire a new consumer into N existing ad hoc loops" is still the only
available pattern.

## The idea

One canonical source of device-state truth for the whole driver, built the
way most systems reconcile reliably against a kernel event source that can
drop events: a push side and a pull side together, not either alone.

- **Push**: a single listener on the kernel's uevent netlink socket
  (`NETLINK_KOBJECT_UEVENT`), filtered to block devices. This needs no udev
  daemon in the container; it's the same socket a udev daemon itself listens
  on, and the node pod's existing privilege (`hostNetwork: true`,
  `privileged`, `SYS_ADMIN`) already reaches it. It gives near-real-time
  notice of a device being added, removed, or changed.
- **Pull**: a coarse periodic sweep of sysfs (the same resolver
  `namespaceIdentity`/`presence.go` already use, which re-scans fresh on
  every call) as a backstop. The uevent socket is multicast and best-effort —
  it can drop events under load or while the listener isn't running (a pod
  restart, a deploy) — so the sweep exists to catch whatever the push side
  missed, the same role `systemd-udevd`'s own coldplug pass plays for it.

Together they produce one stream of device transitions — appeared,
vanished, changed identity — that is the single source of truth for "what
does this node's device state look like right now, and what just changed."
Every current consumer becomes a subscriber that applies its own decision
logic on top of that stream instead of also owning a detection mechanism:

- `presence.go`'s gone-detection: subscribe to "vanished."
- `internal/reconnect`'s path repair: subscribe to "vanished" and "appeared"
  for the subsystems it tracks.
- The alias reconciler: subscribe to "appeared" and "changed identity" for
  staged pNFS volumes.
- Whatever needs this next: subscribes too, instead of becoming poller
  number five.

Each consumer still decides what the event means for it. What stops being
duplicated is the act of noticing that something changed in the first place.

## Scope: this is the NVMe/block side only

`KeepLayoutsPrimed`'s NFS-reconnect detection (`internal/csi/node/pnfs_prime.go`)
watches `/proc/self/mountstats`, the kernel's NFS client transport to the
metadata server — a different subsystem entirely. `/proc/self/mountstats` is
a `seq_file`: its content is generated at read time, never written, so it
cannot be watched with inotify, and there's no documented, stably-named,
unprivileged kernel event source for an NFS transport reconnect the way
uevents exist for block devices (the nearest equivalent is SunRPC
tracepoints, which need tracefs/eBPF access this pod doesn't have and aren't
a stable interface). That side stays a poll of a cheap file. This idea is
about unifying the device-identity side, not replacing every poll in the
driver.

## Why this isn't happening now

The actual failure observed (issue 10) was an alias staying wrong for ten
days, not a sub-second race. A poll-based reconciler at a short interval
already closes that failure mode. Building a netlink listener is new
surface: socket lifecycle, uevent parsing, fan-out to multiple consumers,
and migrating three existing detectors onto it without changing their
current behavior. That's worth doing once the pattern needs a fourth or
fifth poller, or once something needs faster-than-poll-interval reaction
that this fix doesn't need today. The near-term alias reconciler is
explicitly a stopgap in this shape, not the intended end state, and should
say so in its own comments.
