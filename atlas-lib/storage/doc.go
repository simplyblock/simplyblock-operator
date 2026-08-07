// Package storage is one node's storage, as one value.
//
// [Accessor] bundles the resolvers that answer for a single node — subsystems,
// devices, and fabrics when that is remoted — plus the questions that need a
// fresh scan to answer. There are two ways to fill one in, and they are the
// only two: [Local] reads this machine through sysfs, and storagerpc.Remote reads
// another node over a link. A caller is on the node or it is not; nothing
// mixes, so nothing has to reconcile a device resolver pointing at one node
// with a subsystem resolver pointing at another.
//
// This package deliberately carries no gRPC. It is the abstraction and the
// local implementation; the transport lives in storagerpc, which imports this
// one and never the other way round.
//
// Every lookup the two resolvers offer is also flat on the accessor, which is
// the usual way to call them — store.DeviceByUUID, store.SubsystemByNQN,
// store.ListDevices — each named for what it returns. The fields stay exported
// for the times you need to hand an [nvme.DeviceResolver] to something else.
//
//	store := storage.Local(nvme.SysfsConfig{})
//	dev, err := store.DeviceByUUID(ctx, lvolUUID)
//	if err != nil { ... }
//
//	// Before disconnecting: has a namespace joined the subsystem since?
//	shared, err := store.HasCoTenants(ctx, dev)
//
// # Snapshots and scans
//
// The values the resolvers return — [nvme.Device] and friends — are immutable
// snapshots holding no handle back to anything. Questions answerable from a
// snapshot are answered in [nvme] as pure filters ([nvme.Siblings],
// [nvme.CoTenants], [nvme.Device.Accessible]) and cost nothing; one List
// answers them for every device in it.
//
// The methods here are the other kind. [Accessor.Siblings] and
// [Accessor.CoTenants] re-scan, because a teardown has to see the namespace that
// was attached after the snapshot was taken — the one that turns a disconnect
// destructive. They live on Accessor rather than on the device because a rescan
// needs a resolver, and a value that quietly carries one hides what asking
// costs: against a remote node, each of these is a round trip.
//
// So a caller with several questions about one node should List once and use
// the pure filters. These are for the single decisive question, and for the
// case where being current is the whole point.
package storage
