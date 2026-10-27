// The controllers whose power-fail atomicity statement is known not to hold.
//
// A controller cannot report less than one block of power-fail atomicity:
// AWUPF and NAWUPF are 0's based, so 0 is one block. On a 4K-formatted
// namespace that floor reads as "4K writes are atomic" without the device ever
// having promised it, and a virtual NVMe controller that fronts storage it does
// not protect against power loss reports exactly that. The statement then says
// nothing, and only the provider's own documentation does. This list holds the
// devices whose provider documents no such guarantee, so a caller treats them as
// devices that can tear a 4K write whatever they report.

package nvme

import "strings"

// disclaimedAtomicity is keyed by PCI vendor and device ID, lowercase and
// without the 0x prefix, joined by a colon.
var disclaimedAtomicity = map[string]string{
	// The controller every Google Cloud Local SSD is attached through, with
	// the model nvme_card. Google documents no atomic write
	// guarantee for Local SSD, and simplyblock's own deployment guide rules
	// GCP out of production for it; the documented guarantee on GCP is
	// Hyperdisk's (a 128 KB unit within 1 MB boundaries).
	"1ae0:001f": "a Google Cloud Local SSD, whose power-fail atomicity Google does not document",
}

// AtomicityDisclaimed reports whether the controller with the given PCI vendor
// and device IDs is one whose power-fail atomicity statement is not to be
// believed, and why. The IDs are matched as sysfs spells them (0x1ae0) or
// without the prefix, in either case.
func AtomicityDisclaimed(vendor, product string) (string, bool) {
	reason, ok := disclaimedAtomicity[pciID(vendor)+":"+pciID(product)]
	return reason, ok
}

func pciID(id string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(id)), "0x")
}
