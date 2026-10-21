package nvmepr

import (
	"fmt"
	"syscall"

	"github.com/simplyblock/atlas/errs"
)

// The positive results of the kernel's persistent-reservation ioctls, from
// enum pr_status.
const (
	prStatusSuccess             = 0x0
	prStatusIOError             = 0x2
	prStatusReservationConflict = 0x18
	prStatusRetryPathFailure    = 0xe0000
	prStatusPathFastFailed      = 0xf0000
	prStatusPathFailed          = 0x10000
)

// statusError turns the result of one reservation ioctl into an error. The
// kernel returns a status above zero for what the target said, and an errno for
// what it could not ask: the NVMe driver maps Invalid Field, Invalid Opcode,
// and Bad Attributes to EINVAL and an unsupported command to EOPNOTSUPP.
func statusError(op string, status int, errno syscall.Errno) error {
	switch {
	case errno == syscall.EINVAL:
		return fmt.Errorf("%s: %w", op, ErrInvalidField)
	case errno == syscall.EOPNOTSUPP:
		return fmt.Errorf("%s: %w", op, errs.ErrUnsupported)
	case errno != 0:
		return fmt.Errorf("%s: %w", op, errno)
	}
	switch status {
	case prStatusSuccess:
		return nil
	case prStatusReservationConflict:
		return fmt.Errorf("%s: %w", op, ErrReservationConflict)
	case prStatusRetryPathFailure, prStatusPathFastFailed, prStatusPathFailed:
		return fmt.Errorf("%s: %w (status %#x)", op, ErrPathFailed, status)
	default:
		return fmt.Errorf("%s: reservation status %#x", op, status)
	}
}
