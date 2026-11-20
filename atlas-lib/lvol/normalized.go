// Choosing between the handle a volume's spec carries and the normalized one an
// annotation records.
//
// A handle provisioned before the v2 API migration spells its pool as a name,
// and the field it lives in cannot be changed: the API server refuses any edit
// to a PersistentVolume's CSI source or a VolumeSnapshotContent's source. So the
// upgrade resolves the name once and writes the result to metadata, which is
// writable where spec is not, and a reader prefers what it finds there.
//
// The rule lives here, beside ParseHandle, because it is a statement about
// handles rather than about Kubernetes: the same two strings are compared
// wherever they come from, and a second implementation of the comparison would
// be a second opinion about which volume an object names.
//
// design-api-upgrade.md §16.4 is the specification.

package lvol

import (
	"fmt"
	"strings"
)

// Normalized is the handle a reader should use, and what was done to arrive at
// it.
type Normalized struct {
	// Handle is the one to use. It is the field's handle with its pool segment
	// replaced whenever the annotation was taken, and the field's unchanged
	// otherwise.
	Handle Handle

	// FromAnnotation records that the annotation supplied the handle. It is
	// true for an annotation that agreed with the field exactly as well as for
	// one that only normalized the pool, because both are the annotation being
	// honored.
	FromAnnotation bool

	// Ignored says why an annotation was not taken, and is empty when there was
	// none or when it was. It is a sentence rather than a code because its only
	// consumer is a report: a handle nobody can explain is a volume somebody
	// has to go and look at.
	Ignored string
}

// NormalizeHandle chooses between the handle a field carries and the one an
// annotation records, reporting whether a handle could be read at all.
//
// The field is the authority on which cluster and which volume the object
// names, and the annotation may only differ from it in the pool. Anything else
// is refused and reported rather than honored: an annotation is metadata, so
// anybody who may edit an object's labels could otherwise redirect a volume to
// another cluster by writing one.
//
// An absent or blank annotation is no annotation at all rather than a wrong
// one, which is the ordinary state of every object provisioned after the
// boundary and of every cluster nobody has migrated.
//
// It returns false only for a field that carries no readable handle. The
// annotation cannot stand in for one, because it is a record about the field
// rather than a replacement for it: a volume the spec does not name is not one
// metadata may invent.
func NormalizeHandle(field, annotated VolumeHandle) (Normalized, bool) {
	on, wellFormed := ParseHandle(field)
	if !wellFormed {
		return Normalized{}, false
	}

	claimed, wellFormed := ParseHandle(annotated)
	switch {
	case strings.TrimSpace(string(annotated)) == "":
		return Normalized{Handle: on}, true
	case !wellFormed:
		return Normalized{Handle: on, Ignored: fmt.Sprintf(
			"the annotated handle %q is not well formed", annotated)}, true
	case claimed.ClusterID != on.ClusterID:
		return Normalized{Handle: on, Ignored: fmt.Sprintf(
			"the annotated handle names cluster %s and the volume is in %s",
			claimed.ClusterID, on.ClusterID)}, true
	case claimed.VolumeID != on.VolumeID:
		return Normalized{Handle: on, Ignored: fmt.Sprintf(
			"the annotated handle names volume %s and this volume is %s",
			claimed.VolumeID, on.VolumeID)}, true
	}
	return Normalized{Handle: claimed, FromAnnotation: true}, true
}
