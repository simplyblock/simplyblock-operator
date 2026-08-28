package controlplane

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/simplyblock/atlas/errs"
)

// StatusError is a non-success response from the control plane, carrying the
// HTTP status it answered with.
//
// The status is kept as a value rather than folded into a message because what a
// caller should do next depends on it: 503 is worth retrying, 400 never is. That
// decision belongs to the shared classifier (package errs/class), which reads the
// status through the HTTPStatus method — so classifying a status the client has
// never seen before needs no change here.
//
// Where a status has a sentinel meaning, StatusError unwraps to it, so callers
// that only care about that keep matching with errors.Is:
//
//	404 -> errs.ErrNotFound
//	409 -> errs.ErrAlreadyExists
type StatusError struct {
	// Op names the resource or operation that failed, e.g., `storage node "abc"`.
	Op string
	// StatusCode is the HTTP status the control plane returned.
	StatusCode int
	// Body is the response body, trimmed — usually the control plane's own
	// error message.
	Body string
}

// Error implements error.
func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: control-plane returned %d", e.Op, e.StatusCode)
	}
	return fmt.Sprintf("%s: control-plane returned %d: %s", e.Op, e.StatusCode, e.Body)
}

// HTTPStatus returns the status code the control plane answered with. It is the
// method the shared classifier looks for, so any error type able to report an
// HTTP status is classified the same way.
func (e *StatusError) HTTPStatus() int { return e.StatusCode }

// Unwrap returns the atlas sentinel this status stands for, or nil when it has
// none — so errors.Is(err, errs.ErrNotFound) holds for a 404 without the caller
// knowing anything about HTTP.
func (e *StatusError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusNotFound:
		return errs.ErrNotFound
	case http.StatusConflict:
		return errs.ErrAlreadyExists
	}
	return nil
}

// respError turns a non-success control-plane response into a *StatusError. what
// names the resource or operation, e.g., `storage node "abc"`.
func respError(what string, code int, body []byte) error {
	return &StatusError{
		Op:         what,
		StatusCode: code,
		Body:       strings.TrimSpace(string(body)),
	}
}
