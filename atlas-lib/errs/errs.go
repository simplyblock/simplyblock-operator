// Package errs defines sentinel errors shared across atlas so callers can
// match with errors.Is regardless of which package produced them. CSI
// consumers can map these to gRPC status codes at their boundary.
package errs

import "errors"

var (
	// ErrNotFound is returned when a requested resource (volume, device,
	// namespace) does not exist or is not attached on this node.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists is returned when creating a resource that is
	// already present.
	ErrAlreadyExists = errors.New("already exists")
	// ErrNotConnected is returned when an operation needs a live NVMe-oF
	// connection that is absent.
	ErrNotConnected = errors.New("nvme target not connected")
	// ErrUnsupported is returned for operations not supported by the
	// current transport, kernel, or configuration.
	ErrUnsupported = errors.New("unsupported")
	// ErrInvalidResponse is returned when a control-plane response
	// deserializes but does not carry what the client needs from it —
	// typically a version skew between the API and the spec the client was
	// generated from (a renamed or dropped field). Retrying will not help.
	ErrInvalidResponse = errors.New("invalid control-plane response")
)
