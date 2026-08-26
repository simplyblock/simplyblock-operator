// Package lvm runs Linux LVM (device-mapper) commands scoped to a fixed set of
// devices, and answers device-content identity questions about them.
//
// The seam exists because a simplyblock NVMe-oF HA volume surfaces as more
// than one local device node on the client, each presenting byte-identical
// backend content. LVM's default behavior — a system-wide scan of every
// visible device, and existence checks keyed by name rather than by device —
// cannot tell those device nodes apart, and can resolve a PV/VG identity
// against whichever duplicate its cache happens to pick. Every function here
// scopes its command to an explicit device list instead of trusting that
// scan, and answers identity questions from a device's own on-disk content
// rather than a name lookup.
//
// This was found and fixed once already, in the CSI driver's client-side VDO
// support (issue #277): an unscoped pvscan/pvs/vgs hit a genuine "duplicate
// PV" ambiguity between a volume's two HA device nodes, and a name-based `vgs
// <name>` check reported a VG as present when it had never actually been
// created on the device asked about (a host restricting default LVM
// visibility via its own devices file breaks name-only lookups the same way).
// Any future feature assembling an LVM stack on top of simplyblock volumes —
// a striped volume group across several members, for instance — inherits the
// identical hazard the moment it scans devices by name instead of by content.
package lvm
