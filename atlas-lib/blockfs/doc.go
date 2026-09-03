// Package blockfs answers one question about a block device — does it already
// hold somebody's data — and, crucially, keeps that answer apart from "the
// device could not be read."
//
// The distinction is the whole point of the package, because the conventional
// way of asking loses it. `blkid` exits 2 both when a device carries no
// filesystem signature and when its probes came back empty because the reads
// underneath them failed, and k8s.io/mount-utils maps that single exit code to
// "unformatted" and runs mkfs. A volume behind a degraded NVMe-oF path is
// therefore indistinguishable from a blank one: a read that exceeds
// nvme_core.io_timeout, or a controller that has passed its ctrl_loss_tmo,
// fails the probe, and staging the volume then destroys the data it holds.
//
// That is not a hypothetical. It is a production data-loss incident, and it
// reproduces on any host in a few commands: put ext4 on a device, stack a
// dm-flakey table with error_reads over it so reads fail while writes still
// land, and `blkid -p -s TYPE -s PTTYPE -o export` returns exit 2 with empty
// output on a filesystem that is plainly there. Upstream tracks the same defect
// reached through a corrupted primary superblock as
// kubernetes/kubernetes#140376, still open, and neither fix proposed there
// covers a device that will not answer a read at all.
//
// So this package reads the device itself and reports what it found. Callers
// deciding whether to format must treat only StateBlank as an empty device:
// every other state either says the device holds data or says nothing could be
// concluded, and both of those are reasons to stop rather than to guess. The
// asymmetry is deliberate — refusing to stage a volume costs an outage, while
// formatting one costs the data.
//
// # Reading a device that may not answer
//
// A read issued to a block device whose paths are gone cannot be interrupted:
// no timeout, cancellation, or close returns those bytes any sooner, and the
// I/O ends only when the kernel gives up on it. Probe therefore reads on its
// own goroutine, which owns the file and the buffer outright, and reports
// StateUnreadable when the deadline passes. The goroutine may outlive the call;
// it holds one descriptor and 128 KiB until the kernel completes or fails the
// I/O, which nvme_core.io_timeout bounds.
package blockfs
