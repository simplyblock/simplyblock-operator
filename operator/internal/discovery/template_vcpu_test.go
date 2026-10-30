// What the draft proposes for vcpuCount, against what the control plane admits.
//
// The control plane counts logical CPUs (lscpu, os.cpu_count) and refuses a
// node unless it has one CPU beyond the cluster's vCPU count. Where CPU topology
// is on, the node preparation reserves two CPUs for the kubelet and the SPDK pod
// requests its whole core count, so the draft keeps those two back: a 16-CPU
// node drafted at 15 asks for more than its 14 allocatable CPUs and never
// schedules.

package discovery

import (
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// workerOnNode0 is a worker whose chosen disk is on memory node 0, which has
// the logical CPUs and physical cores given.
func workerOnNode0(logical, physical int) Worker {
	r := report("worker-1")
	r.CPU.NUMANodes[0] = nodeprobe.NUMACPUs{Node: 0, OnlineCPUs: rangeOf(0, logical), PhysicalCores: physical}
	return Worker{
		Name:    "worker-1",
		Report:  r,
		Devices: []nodeprobe.Device{disk("nvme0n1", "0000:01:00.0", 0, 1<<40)},
	}
}

func TestTheDraftVCPUCountIsLogicalCPUsLessTheSystemReserve(t *testing.T) {
	for _, testCase := range []struct {
		name              string
		logical, physical int
		want              int32
	}{
		{"hyper-threaded", 16, 8, 14},
		{"no hyper-threading", 8, 8, 6},
		{"too small for the reserve, floored at the minimum", 5, 5, MinimumVCPUCount},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, _ := vcpuCountFor(Plan{Workers: []Worker{workerOnNode0(testCase.logical, testCase.physical)}})
			if got != testCase.want {
				t.Errorf("vcpuCount for %d logical CPUs on %d cores is %d, want %d",
					testCase.logical, testCase.physical, got, testCase.want)
			}
		})
	}
}
