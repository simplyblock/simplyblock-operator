// PeerRegistry is the part of the csi-link hub S3BucketReconciler needs to
// reach a node's CSI plugin: resolving a node name to a live gRPC connection,
// the same join NFSExportReconciler's own PeerRegistry already makes in the
// legacy internal/controller package. A second, separate declaration rather
// than an import, because it is a three-line interface naming only the
// part of *link.Registry this package calls, and both packages running in the
// same cmd/main.go manager is what lets each hold the one *link.Registry the
// csi-link hub constructs (design-bucket-provisioning.md §7.1).
package objectstore

import (
	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/link"
)

// PeerRegistry resolves a linked node to a connection, or link.ErrNoSession
// when it is not currently linked.
type PeerRegistry interface {
	Conn(id link.PeerID) (grpc.ClientConnInterface, error)
}
