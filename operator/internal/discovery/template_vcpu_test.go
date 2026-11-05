// What the draft proposes for vcpuCount, against what the control plane admits.
//
// The draft gives SPDK 10% of the logical CPUs of the socket the storage node
// runs on, at least 5, shared among the storage nodes of that socket. It never
// asks for more than the socket has beyond the two CPUs the node preparation
// reserves for the kubelet, because the SPDK pod requests its whole core count
// and would never schedule, and never less than the API's minimum of 4.

package discovery

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
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

func TestTheDraftVCPUCountIsAShareOfTheSocket(t *testing.T) {
	for _, testCase := range []struct {
		name                   string
		logical, physical, nps int
		want                   int32
	}{
		{"a large socket gets a tenth, not all of it", 320, 160, 1, 32},
		{"64 CPUs", 64, 32, 1, 6},
		{"a tenth below the default minimum is raised to it", 16, 8, 1, DefaultMinimumVCPUCount},
		{"the default minimum still fits beside the reserve", 8, 8, 1, 5},
		{"capped at what the socket has beyond the reserve", 6, 6, 1, 4},
		{"too small for the reserve, floored at the API minimum", 5, 5, 1, MinimumVCPUCount},
		{"two storage nodes share the socket's tenth", 320, 160, 2, 16},
		{"two storage nodes each keep the default minimum", 64, 32, 2, DefaultMinimumVCPUCount},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, _ := vcpuCountFor(Plan{Workers: []Worker{workerOnNode0(testCase.logical, testCase.physical)}}, testCase.nps)
			if got != testCase.want {
				t.Errorf("vcpuCount for %d logical CPUs on %d cores, %d node(s) per socket, is %d, want %d",
					testCase.logical, testCase.physical, testCase.nps, got, testCase.want)
			}
		})
	}
}

func TestAStatedVCPUCountIsKept(t *testing.T) {
	plan := Plan{Workers: []Worker{workerOnNode0(320, 160)}}
	seed := &simplyblockv1alpha2.ClusterTemplate{VCPUCount: ptr.To[int32](24)}
	out, err := ClusterTemplateFor("c", plan, TemplateOptions{Seed: seed})
	if err != nil {
		t.Fatal(err)
	}
	if got := *out.Template.VCPUCount; got != 24 {
		t.Errorf("vcpuCount is %d, want the stated 24", got)
	}
}
