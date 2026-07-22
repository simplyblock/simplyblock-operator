// Package nvme discovers and looks up local NVMe controllers and
// namespaces (for simplyblock, typically NVMe-oF/TCP attachments).
//
// It is read-only: establishing and tearing down fabric connections is
// the job of package nvmeof. The actual enumeration is delegated to
// internal/sysfs so this package stays a stable, testable API surface.
//
// The model types (Subsystem, Controller, Namespace, Path, Device) are
// immutable value snapshots of kernel state at scan time. To observe changed
// state, re-resolve through a resolver rather than mutating a value in place.
//
// # Resolving devices
//
// A resolver reads the local sysfs tree; the zero SysfsConfig uses /sys and
// /dev. Each call re-scans, so results reflect current kernel state.
//
//	ctx := context.Background()
//	devices := nvme.NewSysfsDeviceResolver(nvme.SysfsConfig{})
//
//	// Every attached namespace device.
//	all, err := devices.List(ctx)
//	if err != nil {
//		return err
//	}
//
//	// The device backing one simplyblock volume (lvol UUID == namespace UUID).
//	dev, err := devices.ByUUID(ctx, "fee75e72-1291-4193-8357-3e228ced6c49")
//	if err != nil {
//		return err // wraps errs.ErrNotFound when the volume is not attached
//	}
//	fmt.Println(dev.Namespace.DevicePath) // e.g. /dev/nvme0n1
//
// # Multipath paths
//
// A device's per-controller ANA paths describe multipath/HA reachability;
// the kernel routes I/O to optimized paths first.
//
//	for _, p := range dev.Namespace.Paths {
//		fmt.Printf("%s via %s: %s (accessible=%t)\n",
//			p.Name, p.Controller, p.ANAState, p.ANAState.Accessible())
//	}
//
// # Siblings (same volume)
//
// A volume is identified by its namespace UUID. When native NVMe multipath is
// disabled it can surface as several block devices sharing that UUID;
// Siblings returns the other devices backing the same volume (empty under
// native multipath, where the volume is a single multipath head).
//
//	for _, s := range dev.Siblings(all) {
//	}
//
//	// When you hold a Device and a resolver but not a list, re-scan for a
//	// fresh, coherent snapshot:
//	siblings, err := nvme.SiblingsVia(ctx, devices, dev)
//
// # Multi-namespace subsystems
//
// simplyblock "namespaced" volumes share a single subsystem (created with
// max_namespaces > 1). IsMultiNamespace answers from sysfs when it can — more
// than one namespace, or any NSID > 1 — and only for the ambiguous
// single-namespace-at-NSID-1 case issues an NVMe Identify Controller command
// (Linux only) to read MNAN, the subsystem's maximum allowed namespace count.
//
//	multi, err := dev.IsMultiNamespace()
//	if err != nil {
//		// errs.ErrUnsupported off Linux; errs.ErrNotConnected if no live path
//		return err
//	}
//	if multi {
//		// this namespace shares its subsystem with other volumes
//	}
//
//	// The raw capacity is available per controller:
//	if len(dev.Subsystem.Controllers) > 0 {
//		mnan, _ := dev.Subsystem.Controllers[0].MaxNamespaces() // MNAN from Identify
//		fmt.Println("max namespaces:", mnan)
//	}
//
// Subsystems can be resolved directly too:
//
//	subs := nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{})
//	s, err := subs.ByNQN(ctx, dev.Subsystem.NQN)
package nvme
