// The pNFS volume handle: the identifier an RWX volume is addressed by, and
// the parsing that tells it apart from the three-part lvol handle beside it.
//
// Its own file because the two forms name different things: an lvol handle names
// a volume the control plane owns, a pNFS handle names an export the operator
// owns. Keeping the parsers apart stops an export UUID reaching code that would
// address an lvol with it.
package lvol

import "strings"

// nfsHandlePrefix marks a handle as a pNFS export. Matched exactly: a handle
// differing by case belongs to something else, and guessing which is how one
// volume ends up addressed as another.
const nfsHandlePrefix = "nfs"

// nfsHandleSegments is the segment count of the pNFS form, the prefix included.
const nfsHandleSegments = 4

// NFSHandle is a pNFS volume handle taken apart: the cluster and pool it lives
// in, and the export that serves it.
//
// The export UUID is deliberately not the backing lvol's. An RWX volume has one
// lvol today and would have several once striping arrives, so a handle built on
// the lvol id would change identity the moment that happened. Everything about
// the volume beyond these three ids is read from the NFSExport record the
// export UUID keys.
type NFSHandle struct {
	// ClusterID is a canonical UUID, exactly as the handle spells it. It is not
	// normalized, for the reason Handle gives: a handle is compared against
	// control-plane responses as a string.
	ClusterID string

	// PoolRef is a pool UUID or a pool name, both of which occur, exactly as
	// for the three-part form.
	PoolRef string

	// ExportUUID keys the NFSExport record for this volume.
	ExportUUID string
}

// NewNFSVolumeHandle assembles a pNFS handle from the three ids it encodes. It
// is ParseNFSHandle's inverse, and exists for the reason NewVolumeHandle does:
// the one place that knows the encoding should be the one place that writes it.
//
// The ids are not parsed. A caller holding UUIDs that came from the control
// plane has already established they are UUIDs, and ParseNFSHandle is what
// validates a handle arriving from outside.
func NewNFSVolumeHandle(clusterID, poolRef, exportUUID string) VolumeHandle {
	return VolumeHandle(NFSHandle{
		ClusterID:  clusterID,
		PoolRef:    poolRef,
		ExportUUID: exportUUID,
	}.String())
}

// IsNFS reports whether the handle is shaped like a pNFS one, which is the
// cheap question a caller holding an arbitrary handle asks to pick a parser.
//
// It answers on the prefix alone, so a malformed pNFS handle still reads as one
// and is rejected by ParseNFSHandle, rather than falling through to ParseHandle
// and being mistaken for an lvol handle.
func (h VolumeHandle) IsNFS() bool {
	return strings.HasPrefix(strings.TrimSpace(string(h)), nfsHandlePrefix+handleSeparator)
}

// ParseNFSHandle splits a pNFS volume handle into the cluster, pool, and export
// it names, reporting whether the handle was well formed.
//
// The cluster and export segments must be canonical UUIDs. The pool segment
// only has to be non-empty, since it may be a name. Surrounding whitespace is
// trimmed, for the reason ParseHandle trims it: a handle is read back out of a
// PersistentVolume, which is a YAML document a human may have edited.
//
// A three-part lvol handle is not a pNFS handle and is rejected here, just as
// ParseHandle rejects this form. Neither parser accepts the other's handle,
// which is what lets a caller tell the two apart by asking.
func ParseNFSHandle(h VolumeHandle) (NFSHandle, bool) {
	parts := strings.Split(strings.TrimSpace(string(h)), handleSeparator)
	if len(parts) != nfsHandleSegments || parts[0] != nfsHandlePrefix {
		return NFSHandle{}, false
	}
	clusterID, poolRef, exportUUID := parts[1], parts[2], parts[3]
	if !IsCanonicalUUID(clusterID) || poolRef == "" || !IsCanonicalUUID(exportUUID) {
		return NFSHandle{}, false
	}
	return NFSHandle{ClusterID: clusterID, PoolRef: poolRef, ExportUUID: exportUUID}, true
}

// String renders the handle back into the form ParseNFSHandle reads.
func (h NFSHandle) String() string {
	return strings.Join(
		[]string{nfsHandlePrefix, h.ClusterID, h.PoolRef, h.ExportUUID},
		handleSeparator,
	)
}

// Handle renders the parts into a VolumeHandle.
func (h NFSHandle) Handle() VolumeHandle {
	return VolumeHandle(h.String())
}
