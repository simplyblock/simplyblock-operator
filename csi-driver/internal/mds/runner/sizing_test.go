package runner

import (
	"testing"

	"github.com/simplyblock/csi-driver/internal/mds/qemu"
)

func TestGuestResourcesLeaveTheAllowanceToQEMU(t *testing.T) {
	cases := []struct {
		cpuMilli, memMiB int64
		want             Resources
	}{
		{2000, 2048, Resources{CPUs: 2, MemoryMiB: 2048 - memoryAllowanceMiB}},
		// A fractional core is not a vCPU: the guest gets the whole cores the
		// limit covers, and at least one.
		{1500, 1024, Resources{CPUs: 1, MemoryMiB: 1024 - memoryAllowanceMiB}},
		{500, 1024, Resources{CPUs: 1, MemoryMiB: 1024 - memoryAllowanceMiB}},
		// The smallest limit that still boots a guest.
		{4000, qemu.MinMemoryMiB + memoryAllowanceMiB, Resources{CPUs: 4, MemoryMiB: qemu.MinMemoryMiB}},
	}
	for _, c := range cases {
		got, err := GuestResources(c.cpuMilli, c.memMiB)
		if err != nil || got != c.want {
			t.Errorf("GuestResources(%dm, %dMi) = %+v, %v, want %+v", c.cpuMilli, c.memMiB, got, err, c.want)
		}
	}
}

func TestGuestResourcesRefuseLimitsTooSmallOrUnset(t *testing.T) {
	cases := []struct{ cpuMilli, memMiB int64 }{
		{0, 2048},
		{-1, 2048},
		{2000, 0},
		{2000, qemu.MinMemoryMiB + memoryAllowanceMiB - 1},
	}
	for _, c := range cases {
		if got, err := GuestResources(c.cpuMilli, c.memMiB); err == nil {
			t.Errorf("GuestResources(%dm, %dMi) = %+v, want an error", c.cpuMilli, c.memMiB, got)
		}
	}
}
