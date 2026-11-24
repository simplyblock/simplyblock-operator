//go:build linux

// Tests of how the Reservation Report passthrough reads the ioctl's two failure
// channels, with the syscall stubbed so no NVMe device is needed.

package nvmepr

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func stubSyscall(t *testing.T, r1 uintptr, errno unix.Errno) {
	t.Helper()
	orig := rawSyscall
	rawSyscall = func(_, _, _, _ uintptr) (uintptr, uintptr, unix.Errno) { return r1, 0, errno }
	t.Cleanup(func() { rawSyscall = orig })
}

func TestPassthruCmdHasTheKernelsSize(t *testing.T) {
	if got := unsafe.Sizeof(passthruCmd{}); got != 72 {
		t.Errorf("sizeof(struct nvme_passthru_cmd) = %d, want 72", got)
	}
}

// Regression: a command the controller completes with an error returns its
// NVMe status as a positive value with errno 0. It was read as success, so a
// rejected Reservation Report read as an empty one: no holder, no registrants.
func TestReservationReportReturnsTheControllersRejection(t *testing.T) {
	stubSyscall(t, 0x83, 0) // SCT 0, SC 0x83: Reservation Conflict
	if err := reservationReport(0, 1, make([]byte, 8), 1000); err == nil {
		t.Fatal("reservationReport = nil for NVMe status 0x83, want the controller's rejection")
	}
}

func TestReservationReportSucceedsOnStatusZero(t *testing.T) {
	stubSyscall(t, 0, 0)
	if err := reservationReport(0, 1, make([]byte, 8), 1000); err != nil {
		t.Fatalf("reservationReport = %v for status 0, want nil", err)
	}
}

func TestReservationReportReturnsTheErrno(t *testing.T) {
	stubSyscall(t, 0, unix.EIO)
	if err := reservationReport(0, 1, make([]byte, 8), 1000); err != unix.EIO {
		t.Fatalf("reservationReport = %v, want EIO", err)
	}
}
