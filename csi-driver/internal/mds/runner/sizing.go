// The guest's vCPUs and memory, derived from the MDS pod's limits.
//
// One setting sizes both the pod and the guest (design-pnfs-mds-vm.md §5.5):
// the pod's limits, less a fixed allowance for what runs beside the guest in
// the same cgroup, QEMU's own memory and the runner. A guest sized to the full
// limit would push the cgroup over it and get the whole pod OOM-killed.

package runner

import (
	"fmt"

	"github.com/simplyblock/csi-driver/internal/mds/qemu"
)

// memoryAllowanceMiB is what QEMU and the runner use beside the guest's own
// memory: QEMU's device emulation, its page tables for the guest, and the
// runner's Go heap. It is an estimate until the first boots are measured.
const memoryAllowanceMiB = 256

// Resources is what the guest is started with.
type Resources struct {
	CPUs      int
	MemoryMiB int
}

// GuestResources derives the guest's resources from the pod's CPU limit in
// millicores and memory limit in MiB, the units the downward API delivers
// them in with divisors of 1m and 1Mi.
func GuestResources(cpuLimitMilli, memoryLimitMiB int64) (Resources, error) {
	if cpuLimitMilli <= 0 {
		return Resources{}, fmt.Errorf("CPU limit %dm: the MDS pod needs a CPU limit to size its guest", cpuLimitMilli)
	}
	guestMemory := memoryLimitMiB - memoryAllowanceMiB
	if guestMemory < qemu.MinMemoryMiB {
		return Resources{}, fmt.Errorf("memory limit %dMi leaves the guest %dMi, it needs at least %dMi (limit %dMi)",
			memoryLimitMiB, guestMemory, qemu.MinMemoryMiB, qemu.MinMemoryMiB+memoryAllowanceMiB)
	}
	// Whole cores only, and at least one. QEMU's own threads share the same
	// CPU quota, so a vCPU per partial core would only be throttled.
	cpus := max(1, int(cpuLimitMilli/1000))
	return Resources{CPUs: cpus, MemoryMiB: int(guestMemory)}, nil
}
