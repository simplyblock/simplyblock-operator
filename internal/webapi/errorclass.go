package webapi

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// APIErrorClass classifies a simplyblock control-plane error into retry policy.
//
// Adapted from the CSI driver's controlPlaneErrorClass (pkg/spdk/errorclass.go).
// gRPC codes are omitted since the operator does not speak gRPC.
type APIErrorClass struct {
	// Retryable indicates the operation may succeed if retried (transient failure).
	Retryable bool

	// ContextSpecific indicates the status code meaning depends on the calling
	// operation and cannot be resolved generically:
	//   404 — success for a DELETE, not-found error for a GET or create
	//   409 — idempotent conflict for a create, hard error for other operations
	// Callers must handle these cases explicitly.
	ContextSpecific bool
}

// ClassifyError returns the retry policy for a control-plane error returned by
// apiClient.Do. Pass the HTTP status code returned alongside the error; if err
// is nil and status indicates success, the zero value (not retryable, not
// context-specific) is returned.
func ClassifyError(err error, status int) APIErrorClass {
	if err != nil {
		return classifyTransportError(err)
	}
	return ClassifyStatus(status)
}

// ClassifyStatus classifies a raw HTTP status code without an accompanying error.
func ClassifyStatus(status int) APIErrorClass {
	switch status {
	// Success — no error.
	case http.StatusOK, http.StatusCreated, http.StatusAccepted,
		http.StatusNoContent:
		return APIErrorClass{}

	// Context-specific: meaning depends on the calling operation.
	case http.StatusNotFound,  // 404
		http.StatusConflict: // 409
		return APIErrorClass{ContextSpecific: true}

	// Retryable server errors + request timeout + back-pressure.
	case http.StatusRequestTimeout,      // 408
		http.StatusTooManyRequests,      // 429
		http.StatusInternalServerError,  // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
		http.StatusInsufficientStorage: // 507
		return APIErrorClass{Retryable: true}

	// Permanent server errors.
	case http.StatusNotImplemented,               // 501
		http.StatusHTTPVersionNotSupported,       // 505
		http.StatusLoopDetected,                  // 508
		http.StatusNetworkAuthenticationRequired: // 511
		return APIErrorClass{}

	// Permanent client errors.
	case http.StatusBadRequest,           // 400
		http.StatusUnauthorized,          // 401
		http.StatusForbidden,             // 403
		http.StatusUnprocessableEntity,   // 422
		http.StatusPreconditionFailed:    // 412
		return APIErrorClass{}
	}

	switch {
	case status >= 400 && status < 500:
		return APIErrorClass{} // unlisted 4xx — permanent
	default:
		return APIErrorClass{} // unlisted 5xx — permanent
	}
}

// classifyTransportError classifies a non-HTTP error (network, context).
func classifyTransportError(err error) APIErrorClass {
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled):
		return APIErrorClass{Retryable: true}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return APIErrorClass{Retryable: true}
	}
	// Unknown non-transport error — treat as permanent.
	return APIErrorClass{}
}
