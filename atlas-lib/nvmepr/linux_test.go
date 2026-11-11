//go:build linux

// The Linux device against what the kernel's uapi header fixes: the ioctl
// request numbers, the struct layouts they carry, and the way a file that is
// not a reservation-capable namespace answers.
package nvmepr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

// The request numbers of include/uapi/linux/pr.h: _IOW('p', nr, struct) is
// (1<<30)|(sizeof<<16)|('p'<<8)|nr. The literals are what the header computes,
// written out so a wrong struct size shows up as a wrong number.
func TestIoctlRequestNumbersMatchTheKernelHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"IOC_PR_REGISTER", iocPRRegister, 0x401870c8},
		{"IOC_PR_RESERVE", iocPRReserve, 0x401070c9},
		{"IOC_PR_PREEMPT", iocPRPreempt, 0x401870cb},
		{"IOC_PR_PREEMPT_ABORT", iocPRPreemptAbort, 0x401870cc},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %#x, want %#x", tc.name, tc.got, tc.want)
		}
	}
}

func TestIoctlStructsHaveTheKernelsSizes(t *testing.T) {
	if got := unsafe.Sizeof(prRegistration{}); got != 24 {
		t.Errorf("sizeof(struct pr_registration) = %d, want 24", got)
	}
	if got := unsafe.Sizeof(prReservation{}); got != 16 {
		t.Errorf("sizeof(struct pr_reservation) = %d, want 16", got)
	}
	if got := unsafe.Sizeof(prPreempt{}); got != 24 {
		t.Errorf("sizeof(struct pr_preempt) = %d, want 24", got)
	}
}

// A regular file takes none of the ioctls, so every call fails with ENOTTY and
// the caller gets an error and not a zero result it would read as success.
func TestADeviceThatIsNotANamespaceFailsEveryCommand(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notanamespace")
	if err := os.WriteFile(p, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	dev, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = dev.Close() }()
	ctx := context.Background()

	if err := dev.Register(ctx, 1, 2, true); err == nil {
		t.Error("Register on a regular file succeeded")
	}
	if err := dev.Acquire(ctx, 2, TypeExclusiveAccessRegOnly); err == nil {
		t.Error("Acquire on a regular file succeeded")
	}
	if err := dev.Preempt(ctx, 2, 1, TypeExclusiveAccessRegOnly, true); err == nil {
		t.Error("Preempt on a regular file succeeded")
	}
	if _, err := dev.Report(ctx); err == nil {
		t.Error("Report on a regular file succeeded")
	}
}

func TestOpenOfAMissingDeviceIsAnError(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want one wrapping os.ErrNotExist", err)
	}
}
