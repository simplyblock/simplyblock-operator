// How the kernel's results for a reservation ioctl become errors a caller can
// tell apart: the fence has to know a refused Preempt and Abort (fall back to
// a plain Preempt) from a conflict (the host is not registered as it thought)
// from a path failure (retry).
package nvmepr

import (
	"errors"
	"syscall"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

func TestStatusErrorTellsTheCasesApart(t *testing.T) {
	cases := []struct {
		name   string
		status int
		errno  syscall.Errno
		want   error // nil means no error
	}{
		{"success", 0, 0, nil},
		{"conflict", 0x18, 0, ErrReservationConflict},
		{"invalid field", 0, syscall.EINVAL, ErrInvalidField},
		{"not supported", 0, syscall.EOPNOTSUPP, errs.ErrUnsupported},
		{"path failed", 0x10000, 0, ErrPathFailed},
		{"path fast failed", 0xf0000, 0, ErrPathFailed},
		{"retryable path failure", 0xe0000, 0, ErrPathFailed},
		{"other errno", 0, syscall.EIO, syscall.EIO},
	}
	for _, tc := range cases {
		err := statusError("preempt", tc.status, tc.errno)
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s: err = %v, want nil", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want it to wrap %v", tc.name, err, tc.want)
		}
	}
}

func TestStatusErrorKeepsAnUnknownStatusAsAnError(t *testing.T) {
	err := statusError("register", 0x2, 0)
	if err == nil {
		t.Fatal("an I/O error status read as success")
	}
	for _, sentinel := range []error{ErrReservationConflict, ErrInvalidField, ErrPathFailed} {
		if errors.Is(err, sentinel) {
			t.Errorf("an I/O error status wraps %v", sentinel)
		}
	}
}

// A result that is both an errno and a status is an errno: the kernel never
// returns both, and EINVAL must not be hidden by a stale status value.
func TestStatusErrorPrefersTheErrno(t *testing.T) {
	if err := statusError("preempt", 0x18, syscall.EINVAL); !errors.Is(err, ErrInvalidField) {
		t.Errorf("err = %v, want ErrInvalidField", err)
	}
}
