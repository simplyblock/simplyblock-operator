package nvmepr

import (
	"context"
	"errors"
)

// Device is one namespace's reservation interface, as one host sees it.
type Device interface {
	// Report reads the namespace's reservation status.
	Report(ctx context.Context) (Report, error)

	// Register registers newKey for this host. A zero oldKey is a plain
	// Register, which the target refuses for a host already registered under
	// another key. A non-zero oldKey is a Replace, and with ignoreExistingKey it
	// registers a host that is not registered yet and replaces the key of one
	// that is, whatever oldKey says.
	Register(ctx context.Context, oldKey, newKey uint64, ignoreExistingKey bool) error

	// Acquire reserves the namespace for the host registered under key.
	Acquire(ctx context.Context, key uint64, t Type) error

	// Preempt takes the reservation from the host registered under victim and
	// unregisters it, for the host registered under key. With abort, the target
	// completes the command only once the victim's commands have finished, and
	// a target without it answers ErrInvalidField.
	Preempt(ctx context.Context, key, victim uint64, t Type, abort bool) error

	// Close releases the device.
	Close() error
}

var (
	// ErrReservationConflict is a command the target refused because of the
	// reservation, such as a Register for a host already registered under
	// another key.
	ErrReservationConflict = errors.New("reservation conflict")

	// ErrInvalidField is a command the target does not accept, which is how a
	// target without Preempt and Abort answers it.
	ErrInvalidField = errors.New("reservation command refused: invalid field")

	// ErrPathFailed is a command no path of the namespace could carry.
	ErrPathFailed = errors.New("reservation command failed on every path")
)
