// Package lvm runs Linux LVM (device-mapper) commands scoped to a fixed set of
// devices, and answers device-content identity questions about them.
//
// The seam exists because LVM resolves a PV, VG, or LV identity by scanning
// every device it can see and matching the UUIDs and names it finds in their
// content, not by addressing the device the caller had in mind. That is
// unambiguous as long as no two visible devices carry the same content, which
// is the normal case here: every volume group is named after its own lvol and
// every pvcreate mints a fresh PV UUID, so a node's other tenants cannot
// answer a lookup meant for this volume however many of them are attached.
//
// Cloning breaks that property on purpose. A byte-level clone or snapshot
// restore carries its source's PV and VG UUIDs and its VG name verbatim, so
// from the moment a clone is attached beside its source until
// ImportClonedVolumeGroup has given it fresh identity, the two are the same
// volume group as far as a name lookup is concerned, and LVM resolves against
// whichever one its cache happens to pick. Scoping a command to an explicit
// device list is what distinguishes them, and answering identity questions
// from a device's own on-disk content rather than a name lookup is the same
// defense from the other side. (A stale NVMe controller awaiting reaping
// duplicates content the same way, but that state is detected and killed
// rather than operated in, so nothing here is designed around it.)
//
// This was found and fixed once already, in the CSI driver's client-side VDO
// support (issue #277): an unscoped pvscan/pvs/vgs hit a genuine "duplicate
// PV" ambiguity, and a name-based `vgs <name>` check reported a VG as present
// when it had never actually been created on the device asked about, leaving
// no logical volume behind it and failing mkfs. A host restricting default
// LVM visibility through its own /etc/lvm/devices/system.devices file breaks
// name-only lookups the same way, from the opposite direction.
// Any future feature assembling an LVM stack on top of simplyblock volumes
// inherits the identical hazard the moment it scans devices by name instead of
// by content. A striped volume group across several members would be one.
//
// # Device scoping is internal
//
// Every command here either names a device or does not, and that decides its
// scope without a caller having to think about it. A command with a device
// operand (pvcreate, pvresize, vgcreate, vgextend, pvs, pvscan) is scoped to
// exactly that device, so it cannot resolve against a clone of it. A command
// that addresses a volume group or logical volume by name (vgchange, vgremove,
// lvcreate, lvextend, lvs, lvrename) runs unscoped, because the names are
// unique by the time it runs and scoping it would narrow the scan without
// changing the answer.
//
// That is why no method takes a device list. The scope follows from the
// operands, which the package has, rather than from a judgment about LVM's
// resolution behavior, which a caller would have to make and could only get
// wrong. ImportClonedVolumeGroup is the one operation whose scope decides an
// outcome rather than narrowing a scan, and it derives that scope from the
// device path it is already given.
//
// A simplyblock volume presents one namespace head per volume, with path
// selection handled inside it by ANA state, so the device path a caller hands
// in is that head, the same value it stages, never a per-controller leg. A
// striped volume group passes one path per member, which is what
// CreateVolumeGroup's variadic device-path list is for.
package lvm
