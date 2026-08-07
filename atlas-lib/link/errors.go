package link

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrNoSession is returned when a peer is not currently linked — it has not
// connected yet, it is restarting, or its session dropped and it has not come
// back.
//
// This is an ordinary state, not a fault. Nodes are legitimately unreachable
// for the length of a DaemonSet rollout, and every peer is briefly gone when
// the operator's leadership moves. So it carries codes.Unavailable, which
// errs/class classifies as retryable: a reconciler that runs its errors through
// the classifier requeues with backoff rather than failing the object.
//
// Match it with errors.Is.
var ErrNoSession error = noSessionError{}

type noSessionError struct{}

func (noSessionError) Error() string { return "no live session for peer" }

func (noSessionError) GRPCStatus() *status.Status {
	return status.New(codes.Unavailable, "no live session for peer")
}