//go:build linux

// Tests of how Run reads the ioctl's two failure channels, with the syscall
// stubbed so no NVMe device is needed.
package nvmepassthru

import (
	"testing"

	"golang.org/x/sys/unix"
)

func stubSyscall(t *testing.T, r1 uintptr, errno unix.Errno) {
	t.Helper()
	orig := syscall
	syscall = func(_, _, _, _ uintptr) (uintptr, uintptr, unix.Errno) { return r1, 0, errno }
	t.Cleanup(func() { syscall = orig })
}

// Regression: a command the controller completes with an error returns its
// NVMe status as a positive value with errno 0. Run returned nil for it, so a
// rejected Reservation Report read as an empty one: no holder, no registrants.
func TestRunReturnsTheControllersRejection(t *testing.T) {
	stubSyscall(t, 0x83, 0) // SCT 0, SC 0x83: Reservation Conflict
	if err := Run(0, IoctlIOCmd, &Cmd{}, make([]byte, 8)); err == nil {
		t.Fatal("Run = nil for NVMe status 0x83, want the controller's rejection")
	}
}

func TestRunSucceedsOnStatusZero(t *testing.T) {
	stubSyscall(t, 0, 0)
	if err := Run(0, IoctlIOCmd, &Cmd{}, make([]byte, 8)); err != nil {
		t.Fatalf("Run = %v for status 0, want nil", err)
	}
}

func TestRunReturnsTheErrno(t *testing.T) {
	stubSyscall(t, 0, unix.EIO)
	if err := Run(0, IoctlIOCmd, &Cmd{}, nil); err != unix.EIO {
		t.Fatalf("Run = %v, want EIO", err)
	}
}
