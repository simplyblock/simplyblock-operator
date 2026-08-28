// Package nvme discovers and looks up local NVMe controllers and namespaces
// (for simplyblock, typically NVMe-oF/TCP attachments).
//
// It is read-only, since connecting and disconnecting is package nvmeof, and it
// delegates enumeration to internal/sysfs. The model types (Subsystem,
// Controller, Namespace, Path, Device) are immutable snapshots of kernel state
// at scan time. Re-resolve to observe changes rather than mutating a value.
//
// A Device additionally remembers the resolver it came out of, so a question it
// cannot answer from its own snapshot re-scans without the caller threading a
// resolver back in (Device.HasSiblings). A device assembled by hand carries
// none, so bind one with Device.WithResolver.
//
// # Resolving devices
//
// Resolvers read the local sysfs tree and re-scan per call. The zero
// SysfsConfig uses /sys and /dev.
//
//	devices := nvme.NewSysfsDeviceResolver(nvme.SysfsConfig{})
//	all, err := devices.List(ctx)
//
//	// One volume's device; lvol UUID == namespace UUID. Wraps
//	// errs.ErrNotFound when the volume is not attached.
//	dev, err := devices.ByUUID(ctx, "fee75e72-1291-4193-8357-3e228ced6c49")
//	fmt.Println(dev.Namespace.DevicePath) // /dev/nvme0n1
//
// DeviceSelector combines those keys and, via ListWithSelector, returns every
// match rather than the first, for when "several matched" is meaningful: one
// namespace of a multi-namespace subsystem, or a fresh device beside a stale
// same-NQN one (see nvmeof.WaitForDevice). Filter applies it to a snapshot
// already held.
//
//	sel := nvme.DeviceSelector{NQN: "nqn.2023-02.io.simplyblock:...", NSID: 2}
//	matches, err := devices.ListWithSelector(ctx, sel)
//	matches = sel.Filter(all)
//
// # Reachability
//
// Attached is not usable. Accessible reports whether I/O can be issued: an
// accessible ANA path, or a live owning controller where the kernel publishes
// no ANA view. The By* lookups prefer a reachable match, so a fresh subsystem
// beats the stale one the kernel has yet to reap.
//
//	if !dev.Accessible() { /* present, but every path is lost */ }
//
// # Multipath paths
//
// A device's ANA paths are its multipath/HA legs, and the kernel routes I/O to
// optimized ones first.
//
//	for _, p := range dev.Namespace.Paths {
//		fmt.Println(p.Name, p.Controller, p.ANAState, p.ANAState.Accessible())
//	}
//
// # Siblings (same volume)
//
// With native multipath disabled a volume surfaces as one device per path, all
// sharing its namespace UUID. Siblings are those other devices (none under
// native multipath, which has a single head). Co-tenants are the opposite
// relation: *other* volumes on the same subsystem, the namespaces of a
// multi-namespace subsystem. A teardown asks about both, since every sibling
// has to be released and a co-tenant forbids disconnecting the subsystem, and
// often only needs to know whether there are any.
//
// Each question comes as a pure filter over a snapshot the caller owns, which is
// the cheap form since one List answers all four for every device in it:
//
//	sibs := nvme.Siblings(dev, all)
//	tenants := nvme.CoTenants(dev, all)
//	if nvme.HasSiblings(dev, all) || nvme.HasCoTenants(dev, all) { }
//
// and as a method that re-scans through the resolver the device came from, so
// the answer is current rather than as-of scan time:
//
//	sibs, err := dev.Siblings(ctx)
//	tenants, err := dev.CoTenants(ctx)
//	more, err := dev.HasSiblings(ctx)
//	shared, err := dev.HasCoTenants(ctx)
//
// # Multi-namespace subsystems
//
// simplyblock "namespaced" volumes share one subsystem (max_namespaces > 1).
// IsMultiNamespace answers from sysfs where it can (several NSIDs, or any
// NSID > 1) and only for the ambiguous single-namespace-at-NSID-1 case issues
// an Identify Controller command (Linux only) to read MNAN.
//
//	// errs.ErrUnsupported off Linux; errs.ErrNotConnected without a live path.
//	multi, err := dev.IsMultiNamespace()
//	mnan, err := dev.Subsystem.Controllers[0].MaxNamespaces()
//
// Subsystems resolve directly too:
//
//	subs := nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{})
//	s, err := subs.ByNQN(ctx, dev.Subsystem.NQN)
package nvme
