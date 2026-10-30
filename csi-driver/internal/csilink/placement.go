// The controller's way to the operator's placement service: Place asked over
// the controller's link (design-superclusters.md §7.2). A controller without a
// live session answers Unavailable, so the provisioner retries the claim
// rather than failing it, as it does any other unreachable dependency.

package csilink

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/placement/placementrpc"
)

// Placement asks the operator over Agent's link which member of a supercluster
// a volume goes to.
type Placement struct {
	Agent *link.Agent
}

// Place asks the operator, or answers Unavailable when the link is down.
func (p Placement) Place(ctx context.Context, req placementrpc.Request) (string, error) {
	conn, err := p.Agent.Conn()
	if err != nil {
		return "", status.Errorf(codes.Unavailable, "the link to the operator is down: %v", err)
	}
	return placementrpc.Remote(conn).Place(ctx, req)
}
