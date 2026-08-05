package nvme

import (
	"fmt"
	"strings"
)

// DeviceSelector narrows a set of attached namespace devices. Every field is
// optional and all set fields are ANDed, so the zero selector matches
// everything; each one on its own is a lookup key the resolvers already expose
// (ByUUID, ByNamespace, ByDevicePath), and combined they express the cases a
// single key cannot — notably "namespace N of subsystem NQN", which is how
// simplyblock's namespaced lvols share one subsystem.
//
// UUID comparison is case-insensitive: sysfs reports namespace UUIDs in lower
// case while the control plane may hand them over upper-cased.
type DeviceSelector struct {
	NQN        string      // subsystem NQN
	NSID       NamespaceID // namespace id; 0: any
	UUID       string      // namespace UUID (simplyblock: the lvol UUID)
	DevicePath string      // block device node, e.g. "/dev/nvme0n1"
}

// Matches reports whether d satisfies every field the selector sets.
func (s DeviceSelector) Matches(d Device) bool {
	if s.NQN != "" && d.Subsystem.NQN != s.NQN {
		return false
	}
	if s.NSID != 0 && d.Namespace.ID != s.NSID {
		return false
	}
	if s.UUID != "" && !strings.EqualFold(d.Namespace.UUID, s.UUID) {
		return false
	}
	if s.DevicePath != "" && d.Namespace.DevicePath != s.DevicePath {
		return false
	}
	return true
}

// Filter returns the devices in devs that the selector matches, keeping their
// order. It is the in-memory counterpart of
// DeviceResolver.ListWithSelector, for callers that already hold a snapshot.
func (s DeviceSelector) Filter(devs []Device) []Device {
	out := make([]Device, 0, len(devs))
	for _, d := range devs {
		if s.Matches(d) {
			out = append(out, d)
		}
	}
	return out
}

// IsZero reports whether the selector constrains nothing, i.e. matches every
// device.
func (s DeviceSelector) IsZero() bool {
	return s == DeviceSelector{}
}

// String renders the set fields as a compact "k=v,..." list, for error
// messages and logs. The zero selector renders as "any".
func (s DeviceSelector) String() string {
	var parts []string
	if s.NQN != "" {
		parts = append(parts, "nqn="+s.NQN)
	}
	if s.NSID != 0 {
		parts = append(parts, fmt.Sprintf("nsid=%d", s.NSID))
	}
	if s.UUID != "" {
		parts = append(parts, "uuid="+s.UUID)
	}
	if s.DevicePath != "" {
		parts = append(parts, "device="+s.DevicePath)
	}
	if len(parts) == 0 {
		return "any"
	}
	return strings.Join(parts, ",")
}
