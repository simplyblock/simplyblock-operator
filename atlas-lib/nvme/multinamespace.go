package nvme

import (
	"fmt"

	"github.com/simplyblock/atlas/errs"
)

// controllerStateLive is the kernel "state" value of a controller that can
// serve admin/IO commands (the only state we can Identify against).
const controllerStateLive = "live"

// identifyMNAN reads the Identify Controller MNAN field for the controller
// character device at devicePath. It is a package variable so tests can
// substitute the ioctl (identifyControllerMNAN is Linux-only).
var identifyMNAN = identifyControllerMNAN

// IsMultiNamespace reports whether this subsystem can host more than one
// namespace. simplyblock's "namespaced" lvols share a single subsystem across
// many volumes (created with max_namespaces > 1); a plain lvol gets a
// subsystem of its own (max_namespaces == 1).
//
// The answer comes from sysfs alone whenever possible — more than one
// namespace attached, or any namespace with NSID > 1, is conclusive. Only the
// single-namespace-at-NSID-1 case is ambiguous: in sysfs it is byte-for-byte
// identical to a plain single-namespace subsystem (same NQN shape, same model,
// one namespace at nsid 1). There the method issues an NVMe Identify Controller
// command against a live controller and reads MNAN — the maximum allowed
// namespace count, which SPDK sets to the subsystem's max_namespaces.
// MNAN > 1 ⇒ multi-namespace.
//
// The Identify fallback is Linux-only (returns errs.ErrUnsupported elsewhere)
// and needs a live controller (returns errs.ErrNotConnected when the subsystem
// has none). The conclusive sysfs cases never touch the device and never error.
func (s Subsystem) IsMultiNamespace() (bool, error) {
	if len(s.Namespaces) > 1 {
		return true, nil
	}
	for _, ns := range s.Namespaces {
		if ns.ID > 1 {
			return true, nil
		}
	}

	ctrl, ok := s.liveController()
	if !ok {
		return false, fmt.Errorf("subsystem %s: cannot read Identify: %w", s.ID, errs.ErrNotConnected)
	}
	nn, err := ctrl.MaxNamespaces()
	if err != nil {
		return false, fmt.Errorf("subsystem %s: %w", s.ID, err)
	}
	return nn > 1, nil
}

// IsLive reports whether the controller is in the kernel "live" state — able
// to serve admin and I/O commands (as opposed to "connecting", "resetting",
// "deleting", etc.).
func (c Controller) IsLive() bool {
	return c.State == controllerStateLive
}

// liveController returns any live controller fronting the subsystem — every
// controller of a subsystem shares its max_namespaces, so any live one answers
// the Identify equally.
func (s Subsystem) liveController() (Controller, bool) {
	for _, c := range s.Controllers {
		if c.IsLive() && c.DevicePath != "" {
			return c, true
		}
	}
	return Controller{}, false
}

// MaxNamespaces returns the maximum number of namespaces the controller's
// subsystem may hold — the MNAN field of its Identify Controller data, which
// SPDK sets to the subsystem's max_namespaces. It issues an NVMe Identify
// Controller admin command against the controller character device, so the
// controller must be live (errs.ErrNotConnected otherwise) and the platform
// Linux (errs.ErrUnsupported otherwise). A subsystem whose controllers report
// MNAN > 1 is multi-namespace; see Subsystem.IsMultiNamespace.
func (c Controller) MaxNamespaces() (uint32, error) {
	if c.State != controllerStateLive {
		return 0, fmt.Errorf("controller %s state %q: %w", c.ID, c.State, errs.ErrNotConnected)
	}
	if c.DevicePath == "" {
		return 0, fmt.Errorf("controller %s: no device path: %w", c.ID, errs.ErrNotConnected)
	}
	mnan, err := identifyMNAN(c.DevicePath)
	if err != nil {
		return 0, fmt.Errorf("controller %s identify: %w", c.ID, err)
	}
	return mnan, nil
}

// IsMultiNamespace reports whether this device's namespace belongs to a
// multi-namespace subsystem. An NSID > 1 is conclusive with no device I/O;
// otherwise it defers to Subsystem.IsMultiNamespace (which may Identify).
func (d Device) IsMultiNamespace() (bool, error) {
	if d.Namespace.ID > 1 {
		return true, nil
	}
	return d.Subsystem.IsMultiNamespace()
}
