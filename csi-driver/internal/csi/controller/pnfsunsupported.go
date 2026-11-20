// The one operation a pNFS volume cannot serve yet.
//
// The list is short because the volume handle is the backing volume's: a
// snapshot, a clone, a capability check, and an expand all address the lvol
// and work without knowing an export is in front of it.

package controller

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// refusePNFSCloneTarget turns away a claim that asks for a pNFS export and
// names a data source.
//
// Refused because it would otherwise half-work rather than fail: the clone
// carries the source's filesystem, so assembly finds it non-blank, skips the
// mkfs, and mounts it as XFS -- which it may not be. What a cloned export
// should contain is a question this phase does not answer.
func refusePNFSCloneTarget(pnfs bool) error {
	if !pnfs {
		return nil
	}
	return status.Error(codes.Unimplemented,
		"a pNFS volume cannot be created from a data source yet: the clone would carry the "+
			"source's filesystem, and what an export made that way should contain is not settled")
}
