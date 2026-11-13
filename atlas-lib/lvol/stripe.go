// The stripe marker a volume or snapshot handle carries when it names a striped
// volume: several logical volumes on different storage nodes that a host
// assembles into one block device with LVM striping.
//
// A striped volume's handle is its first member's handle with a fourth segment
// naming the width, {cluster}:{pool}:{lvol}:s{width}. The members cannot all be
// spelled into the handle, because the CSI specification caps a volume id at
// 128 bytes and three UUIDs already take 111 of them. The first member is the
// volume's identity, and the other members are found from it by name. The width
// is in the handle because the RPCs that act on every member (delete, expand,
// snapshot) receive nothing but the id, and a handle that says it is striped
// lets them tell so without asking the control plane.
//
// ParseHandle does not accept the marker, and that is deliberate: a consumer
// that does not know about striping and reads a striped handle as a plain one
// would act on the first member alone. A consumer that knows calls
// ParseStripedHandle, which reads both forms.

package lvol

import (
	"strconv"
	"strings"
)

// MaxStripeWidth is the most members a striped volume may have. It is LVM's
// own ceiling on lvcreate -i, so a wider volume could be provisioned and then
// never staged.
const MaxStripeWidth = 128

// stripeMarkerPrefix opens the fourth segment of a striped handle.
const stripeMarkerPrefix = "s"

// ParseStripedHandle reads a volume handle that may carry a stripe marker. It
// returns the first member's handle and the width, which is 1 for a handle
// without a marker, and reports whether the handle was well formed.
//
// A handle without a marker parses exactly as ParseHandle reads it.
func ParseStripedHandle(h VolumeHandle) (Handle, int, bool) {
	base, width, ok := SplitStripeMarker(string(h))
	if !ok {
		return Handle{}, 0, false
	}
	handle, ok := ParseHandle(VolumeHandle(base))
	if !ok {
		return Handle{}, 0, false
	}
	return handle, width, true
}

// SplitStripeMarker separates a trailing stripe marker from an identifier made
// of colon-separated segments, a volume handle or a snapshot id. It returns the
// identifier without the marker and the width the marker names, 1 when there is
// no marker.
//
// Only a fourth segment can be a marker. A three-segment identifier is returned
// unchanged with width 1, whatever its last segment says, so no identifier this
// driver issued before striping existed reads differently now. A fourth segment
// that is not a well-formed marker, a width outside 2..MaxStripeWidth included,
// reports false.
func SplitStripeMarker(id string) (string, int, bool) {
	trimmed := strings.TrimSpace(id)
	parts := strings.Split(trimmed, handleSeparator)
	if len(parts) != 4 {
		return trimmed, 1, true
	}
	width, ok := parseStripeMarker(parts[3])
	if !ok {
		return "", 0, false
	}
	return strings.Join(parts[:3], handleSeparator), width, true
}

// parseStripeMarker reads s{width}. The width is decimal with no sign and no
// leading zero, so one width has one spelling and two handles naming the same
// volume compare equal as strings.
func parseStripeMarker(segment string) (int, bool) {
	digits, ok := strings.CutPrefix(segment, stripeMarkerPrefix)
	if !ok || digits == "" || digits[0] == '0' {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	width, err := strconv.Atoi(digits)
	if err != nil || width < 2 || width > MaxStripeWidth {
		return 0, false
	}
	return width, true
}

// WithStripeMarker appends the marker for width to an identifier. A width below
// 2 is not a striped volume, and the identifier is returned unchanged.
func WithStripeMarker(id string, width int) string {
	if width < 2 {
		return id
	}
	return id + handleSeparator + stripeMarkerPrefix + strconv.Itoa(width)
}

// StripedString renders a handle with the marker for width, or as String does
// for a width below 2.
func (h Handle) StripedString(width int) string {
	return WithStripeMarker(h.String(), width)
}
