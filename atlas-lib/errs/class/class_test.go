package class

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/errs"
)

// httpErr is a minimal error carrying an HTTP status, standing in for
// controlplane.StatusError (which this package must not import).
type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string   { return fmt.Sprintf("control-plane returned %d: %s", e.code, e.msg) }
func (e *httpErr) HTTPStatus() int { return e.code }

// timeoutErr is a net.Error that timed out.
type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

// Temporary is deprecated but still part of net.Error.
func (timeoutErr) Temporary() bool { return true }

// refusedErr is a net.Error that did not time out.
type refusedErr struct{}

func (refusedErr) Error() string   { return "connection refused" }
func (refusedErr) Timeout() bool   { return false }
func (refusedErr) Temporary() bool { return false }

var _ net.Error = timeoutErr{}
var _ net.Error = refusedErr{}

func TestOf_HTTPStatuses(t *testing.T) {
	for _, tc := range []struct {
		status    int
		code      codes.Code
		retryable bool
		rpc       bool
	}{
		{404, codes.Internal, false, true}, // operation-specific
		{409, codes.Internal, false, true}, // operation-specific
		{408, codes.Unavailable, true, false},
		{500, codes.Unavailable, true, false},
		{502, codes.Unavailable, true, false},
		{503, codes.Unavailable, true, false},
		{504, codes.Unavailable, true, false},
		{429, codes.ResourceExhausted, true, false},
		{507, codes.ResourceExhausted, true, false},
		{501, codes.Internal, false, false},
		{505, codes.Internal, false, false},
		{400, codes.InvalidArgument, false, false},
		{422, codes.InvalidArgument, false, false},
		{401, codes.Unauthenticated, false, false},
		{403, codes.PermissionDenied, false, false},
		{412, codes.FailedPrecondition, false, false},
		{418, codes.FailedPrecondition, false, false}, // unlisted 4xx
		{599, codes.Internal, false, false},           // unlisted 5xx
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			got := Of(&httpErr{code: tc.status, msg: "boom"})
			if got.Code != tc.code || got.Retryable != tc.retryable || got.RPCSpecific != tc.rpc {
				t.Errorf("Of(%d) = %+v, want code=%v retryable=%v rpcSpecific=%v",
					tc.status, got, tc.code, tc.retryable, tc.rpc)
			}
			if got.Success || got.Idempotent {
				t.Error("the generic classifier must never set Success or Idempotent")
			}
		})
	}
}

// A wrapped status error must classify like a bare one, because errors in atlas
// are wrapped with context on the way up.
func TestOf_ClassifiesThroughWrapping(t *testing.T) {
	err := fmt.Errorf("create volume %q: %w", "vol-a",
		fmt.Errorf("request failed: %w", &httpErr{code: 503, msg: "upstream down"}))
	if got := Of(err); got.Code != codes.Unavailable || !got.Retryable {
		t.Errorf("Of(wrapped 503) = %+v, want Unavailable retryable", got)
	}
}

func TestOf_Sentinels(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code codes.Code
	}{
		{"not found", fmt.Errorf("volume: %w", errs.ErrNotFound), codes.NotFound},
		{"already exists", fmt.Errorf("pool: %w", errs.ErrAlreadyExists), codes.AlreadyExists},
		{"not connected", fmt.Errorf("subsystem: %w", errs.ErrNotConnected), codes.FailedPrecondition},
		{"unsupported", fmt.Errorf("identify: %w", errs.ErrUnsupported), codes.Unimplemented},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Of(tc.err)
			if got.Code != tc.code {
				t.Errorf("code = %v, want %v", got.Code, tc.code)
			}
			if got.Retryable {
				t.Error("a sentinel failure is permanent")
			}
		})
	}
}

// A 404 from the control plane unwraps to ErrNotFound for callers that match on
// it, but classification must still treat it as operation-specific rather than
// answering NotFound generically.
func TestOf_HTTPStatusWinsOverItsSentinel(t *testing.T) {
	err := &statusWithSentinel{httpErr{code: 404, msg: "no such volume"}}
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatal("fixture must unwrap to errs.ErrNotFound")
	}
	if got := Of(err); !got.RPCSpecific || got.Code != codes.Internal {
		t.Errorf("Of(404) = %+v, want RPCSpecific with code Internal", got)
	}
}

type statusWithSentinel struct{ httpErr }

func (e *statusWithSentinel) Unwrap() error { return errs.ErrNotFound }

func TestOf_TransportAndContext(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		code      codes.Code
		retryable bool
	}{
		{"deadline", fmt.Errorf("call: %w", context.DeadlineExceeded), codes.DeadlineExceeded, true},
		{"cancelled", fmt.Errorf("call: %w", context.Canceled), codes.Canceled, true},
		{"net timeout", fmt.Errorf("dial: %w", timeoutErr{}), codes.DeadlineExceeded, true},
		{"connection refused", fmt.Errorf("dial: %w", refusedErr{}), codes.Unavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Of(tc.err)
			if got.Code != tc.code || got.Retryable != tc.retryable {
				t.Errorf("Of = %+v, want code=%v retryable=%v", got, tc.code, tc.retryable)
			}
		})
	}
}

// The rule that keeps a permanent bug from being retried forever.
func TestOf_UnknownErrorIsInternalAndPermanent(t *testing.T) {
	got := Of(errors.New("json: cannot unmarshal number into field"))
	if got.Code != codes.Internal || got.Retryable {
		t.Errorf("Of(unknown) = %+v, want Internal and not retryable", got)
	}
	if !got.Permanent() {
		t.Error("Permanent() = false, want true")
	}
}

func TestOf_Nil(t *testing.T) {
	if got := Of(nil); got.Code != codes.OK || got.Retryable {
		t.Errorf("Of(nil) = %+v, want OK", got)
	}
}

func TestStatus(t *testing.T) {
	t.Run("classifies and keeps the message", func(t *testing.T) {
		err := Status(fmt.Errorf("volume %q: %w", "vol-a", errs.ErrNotFound))
		s, ok := status.FromError(err)
		if !ok || s.Code() != codes.NotFound {
			t.Fatalf("status = %v, want NotFound", s)
		}
		if s.Message() != `volume "vol-a": not found` {
			t.Errorf("message = %q, want the original", s.Message())
		}
	})

	t.Run("passes an existing status through", func(t *testing.T) {
		orig := status.Error(codes.ResourceExhausted, "quota exceeded")
		if got := Status(orig); got != orig {
			t.Errorf("Status rewrote an existing status error: %v", got)
		}
	})

	t.Run("nil stays nil", func(t *testing.T) {
		if err := Status(nil); err != nil {
			t.Errorf("Status(nil) = %v, want nil", err)
		}
	})
}

// The operator/CSI link: what one side classifies, the other must be able to
// match with errors.Is.
func TestRoundTripAcrossTheWire(t *testing.T) {
	for _, sentinel := range []error{
		errs.ErrNotFound,
		errs.ErrAlreadyExists,
		errs.ErrNotConnected,
		errs.ErrUnsupported,
		context.Canceled,
		context.DeadlineExceeded,
	} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			sent := Status(fmt.Errorf("op failed: %w", sentinel))
			received := FromStatus(sent)
			if !errors.Is(received, sentinel) {
				t.Errorf("received %v (%T), want it to match %v", received, received, sentinel)
			}
			if received.Error() != "op failed: "+sentinel.Error() {
				t.Errorf("message = %q, want the sender's", received.Error())
			}
		})
	}
}

func TestFromStatus_PassesThroughWhatItCannotMap(t *testing.T) {
	t.Run("no sentinel for the code", func(t *testing.T) {
		orig := status.Error(codes.Unavailable, "upstream down")
		if got := FromStatus(orig); got != orig {
			t.Errorf("FromStatus = %v, want the original error", got)
		}
	})

	t.Run("not a status error", func(t *testing.T) {
		orig := errors.New("plain")
		if got := FromStatus(orig); got != orig {
			t.Errorf("FromStatus = %v, want the original error", got)
		}
	})

	t.Run("nil", func(t *testing.T) {
		if got := FromStatus(nil); got != nil {
			t.Errorf("FromStatus(nil) = %v, want nil", got)
		}
	})
}

// A status that arrives already classified keeps its code and its retryability.
func TestOf_PreClassifiedStatus(t *testing.T) {
	got := Of(status.Error(codes.Unavailable, "upstream down"))
	if got.Code != codes.Unavailable || !got.Retryable {
		t.Errorf("Of(status Unavailable) = %+v, want Unavailable retryable", got)
	}
	if perm := Of(status.Error(codes.InvalidArgument, "bad size")); perm.Retryable {
		t.Errorf("Of(status InvalidArgument) = %+v, want permanent", perm)
	}
}

func TestCodeAndRetryableHelpers(t *testing.T) {
	err := &httpErr{code: 503, msg: "down"}
	if Code(err) != codes.Unavailable {
		t.Errorf("Code = %v, want Unavailable", Code(err))
	}
	if !Retryable(err) {
		t.Error("Retryable = false, want true")
	}
}
