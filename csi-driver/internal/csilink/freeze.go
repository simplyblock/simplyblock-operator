// The controller's way to a striped volume's freeze: Freeze and Thaw asked of
// the operator over the controller's link, which relays them to the node that
// stages the volume (atlas-lib/freeze/freezerpc). A controller without a live
// session answers Unavailable, which the snapshot retries.

package csilink

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/freeze/freezerpc"
	"github.com/simplyblock/atlas/link"
)

// Freezer asks the operator over Agent's link to freeze and thaw a volume on a
// node.
type Freezer struct {
	Agent *link.Agent
}

// Freeze asks node to suspend the volume, or answers Unavailable when the link
// is down.
func (f Freezer) Freeze(ctx context.Context, node, handle, token string, deadline time.Duration) (bool, error) {
	conn, err := f.Agent.Conn()
	if err != nil {
		return false, status.Errorf(codes.Unavailable, "the link to the operator is down: %v", err)
	}
	return freezerpc.Remote(conn).Freeze(ctx, node, handle, token, deadline)
}

// Thaw asks node to resume the volume and reports whether the freeze held.
func (f Freezer) Thaw(ctx context.Context, node, handle, token string) (bool, error) {
	conn, err := f.Agent.Conn()
	if err != nil {
		return false, status.Errorf(codes.Unavailable, "the link to the operator is down: %v", err)
	}
	return freezerpc.Remote(conn).Thaw(ctx, node, handle, token)
}
