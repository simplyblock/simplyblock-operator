// What the draft proposes for vcpuCount, against what the control plane admits.
//
// The control plane counts logical CPUs (lscpu, os.cpu_count) and refuses a
// node unless it has one CPU beyond the cluster's vCPU count.

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

func TestTheDraftVCPUCountIsLogicalCPUsLessOneForTheSystem(t *testing.T) {
	for _, testCase := range []struct {
		name              string
		logical, physical int
		want              int32
	}{
		{"hyper-threaded", 16, 8, 15},
		{"no hyper-threading", 8, 8, 7},
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
