// Package class classifies an error into what a caller should do about it: the
// gRPC status code to answer with, and whether retrying can help.
//
// Both consumers face the same question and used to answer it separately — the
// CSI driver mapping control-plane failures to RPC statuses, the operator
// deciding whether to requeue a reconcile. Answering it in one place is the point
// of a classifier: it is how a 503 stays retryable and a 400 stays permanent
// everywhere, instead of one component treating a permanent failure as transient
// and retrying it forever.
//
// The policy, for operation-INDEPENDENT failures:
//
//   - Transport: timeout -> DeadlineExceeded, cancel -> Canceled, any other
//     net.Error (connection refused/reset, TLS, DNS) -> Unavailable. Retryable.
//   - 500/502/503/504 and 408 -> Unavailable. Retryable.
//   - 429 and 507 -> ResourceExhausted. Retryable backpressure.
//   - 400/422 -> InvalidArgument, 401 -> Unauthenticated, 403 -> PermissionDenied,
//     412 -> FailedPrecondition, any other 4xx -> FailedPrecondition. Permanent.
//   - 501/505/508/511 and any other 5xx -> Internal. Permanent.
//   - atlas sentinels: ErrNotFound -> NotFound, ErrAlreadyExists -> AlreadyExists,
//     ErrNotConnected -> FailedPrecondition, ErrUnsupported -> Unimplemented.
//   - Anything unrecognized -> Internal, NOT retryable. An error the classifier
//     cannot place is a fault, not a transient failure; calling it Unavailable is
//     what lets a sidecar retry a permanent failure until something collapses.
//
// 404 and 409 are deliberately not classified generically: their meaning depends
// on the operation. A 404 is success for a delete and NotFound for a create from
// a snapshot; a 409 is idempotent-success for a create with the same source and
// AlreadyExists otherwise. Of marks them RPCSpecific with Code Internal, so an
// unresolved one surfaces as a bug in the caller rather than being silently
// mislabeled. A per-operation layer resolves them and fills in Success or
// Idempotent.
package class

import (
	"context"
	"errors"
	"net"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/errs"
)

// Class is what to do about an error.
type Class struct {
	// Code is the gRPC status code to answer with.
	Code codes.Code
	// Retryable reports whether retrying the operation can help.
	Retryable bool

	// RPCSpecific marks a status whose meaning depends on the operation (404,
	// 409) and that a generic classifier therefore cannot resolve. Code is
	// Internal so an unresolved one is visible instead of being mistaken for a
	// retryable failure.
	RPCSpecific bool

	// Success means the error is a no-op for this operation, which should report
	// success — e.g. a 404 on a delete: the object is already gone. Only a
	// per-operation layer sets it; Of never does.
	Success bool

	// Idempotent means the operation must resolve a conflict by looking up the
	// existing object before deciding — e.g. a 409 on a create: same source and
	// parameters means return the existing object, otherwise AlreadyExists.
	// Classification cannot decide that; only a per-operation layer sets it.
	Idempotent bool
}

// Permanent reports whether retrying is pointless.
func (c Class) Permanent() bool { return !c.Retryable }

// httpStatuser is implemented by errors that carry an HTTP status —
// controlplane.StatusError does. The classifier looks for the method rather than
// a concrete type so it works for any client that reports one, and so this
// package depends on no other atlas package than errs.
type httpStatuser interface{ HTTPStatus() int }

// Of classifies err. A nil err is codes.OK.
func Of(err error) Class {
	if err == nil {
		return Class{Code: codes.OK}
	}

	// An HTTP status is the most specific thing an error can carry, so it wins
	// over the sentinel it may also unwrap to: 404 and 409 need per-operation
	// resolution, which the sentinels alone cannot express.
	var hs httpStatuser
	if errors.As(err, &hs) {
		if code := hs.HTTPStatus(); code != 0 {
			return ofHTTPStatus(code)
		}
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Class{Code: codes.DeadlineExceeded, Retryable: true}
	case errors.Is(err, context.Canceled):
		return Class{Code: codes.Canceled, Retryable: true}
	case errors.Is(err, errs.ErrNotFound):
		return Class{Code: codes.NotFound}
	case errors.Is(err, errs.ErrAlreadyExists):
		return Class{Code: codes.AlreadyExists}
	case errors.Is(err, errs.ErrNotConnected):
		return Class{Code: codes.FailedPrecondition}
	case errors.Is(err, errs.ErrUnsupported):
		return Class{Code: codes.Unimplemented}
	}

	// A recognized transport failure is transient.
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return Class{Code: codes.DeadlineExceeded, Retryable: true}
		}
		return Class{Code: codes.Unavailable, Retryable: true}
	}

	// An error that already carries a gRPC status keeps its code — it was
	// classified by whoever produced it, quite possibly this package on the
	// other side of a link.
	if s, ok := statusOf(err); ok {
		return Class{Code: s.Code(), Retryable: retryableCode(s.Code())}
	}

	// Unknown, non-transport error: a fault, not a retry.
	return Class{Code: codes.Internal}
}

// Code is Of(err).Code, for callers that need nothing else.
func Code(err error) codes.Code { return Of(err).Code }

// Retryable is Of(err).Retryable — the operator's requeue-or-fail decision.
func Retryable(err error) bool { return Of(err).Retryable }

// Status returns err as a gRPC status error carrying Of(err).Code and err's
// message: what an RPC handler returns. A nil err stays nil, and an err that
// already carries a status is passed through so a code set downstream is not
// overwritten.
func Status(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := statusOf(err); ok {
		return err
	}
	return status.Error(Code(err), err.Error())
}

// FromStatus maps a gRPC status error received from a peer back to the atlas
// sentinel it stands for, keeping the peer's message. It is the reverse of
// Status, so a client of the operator/CSI link can match the same errors.Is
// conditions it would in process. A code with no sentinel yields err unchanged:
// inventing one would claim a meaning the peer never sent.
func FromStatus(err error) error {
	if err == nil {
		return nil
	}
	s, ok := statusOf(err)
	if !ok {
		return err
	}
	sentinel := sentinelFor(s.Code())
	if sentinel == nil {
		return err
	}
	return peerError{msg: s.Message(), err: sentinel}
}

// ofHTTPStatus applies the policy to a raw HTTP status.
func ofHTTPStatus(code int) Class {
	switch code {
	// Operation-specific — only a per-operation layer can resolve these.
	case http.StatusNotFound, // 404
		http.StatusConflict: // 409
		return Class{Code: codes.Internal, RPCSpecific: true}

	// Retryable server errors and request timeout.
	case http.StatusInternalServerError, // 500
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout,     // 504
		http.StatusRequestTimeout:     // 408
		return Class{Code: codes.Unavailable, Retryable: true}

	// Backpressure / capacity — retry with backoff.
	case http.StatusTooManyRequests, // 429
		http.StatusInsufficientStorage: // 507
		return Class{Code: codes.ResourceExhausted, Retryable: true}

	// Permanent server errors.
	case http.StatusNotImplemented, // 501
		http.StatusHTTPVersionNotSupported,       // 505
		http.StatusLoopDetected,                  // 508
		http.StatusNetworkAuthenticationRequired: // 511
		return Class{Code: codes.Internal}

	// Permanent client errors with a specific code.
	case http.StatusBadRequest, http.StatusUnprocessableEntity: // 400, 422
		return Class{Code: codes.InvalidArgument}
	case http.StatusUnauthorized: // 401
		return Class{Code: codes.Unauthenticated}
	case http.StatusForbidden: // 403
		return Class{Code: codes.PermissionDenied}
	case http.StatusPreconditionFailed: // 412
		return Class{Code: codes.FailedPrecondition}
	}

	switch {
	case code >= 400 && code < 500:
		return Class{Code: codes.FailedPrecondition} // unlisted 4xx: permanent
	default:
		return Class{Code: codes.Internal} // unlisted 5xx and anything else
	}
}

// retryableCode reports whether a gRPC code describes a transient failure. It is
// the reverse of the policy above, for a status that arrives already classified.
func retryableCode(code codes.Code) bool {
	switch code {
	case codes.Unavailable, codes.ResourceExhausted, codes.DeadlineExceeded, codes.Canceled:
		return true
	}
	return false
}

func sentinelFor(code codes.Code) error {
	switch code {
	case codes.NotFound:
		return errs.ErrNotFound
	case codes.AlreadyExists:
		return errs.ErrAlreadyExists
	case codes.FailedPrecondition:
		return errs.ErrNotConnected
	case codes.Unimplemented:
		return errs.ErrUnsupported
	case codes.Canceled:
		return context.Canceled
	case codes.DeadlineExceeded:
		return context.DeadlineExceeded
	}
	return nil
}

// statusOf returns the gRPC status err actually carries. status.FromError
// classifies *any* non-nil error (an unclassified one as codes.Unknown), so it
// cannot be used to tell "carries a status" from "is an ordinary error"; the
// GRPCStatus method can.
func statusOf(err error) (*status.Status, bool) {
	var gs interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &gs) {
		return nil, false
	}
	return gs.GRPCStatus(), true
}

// peerError carries a peer's message while matching an atlas sentinel.
type peerError struct {
	msg string
	err error
}

func (e peerError) Error() string { return e.msg }
func (e peerError) Unwrap() error { return e.err }
