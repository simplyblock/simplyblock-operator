// Preflight checks that read what the probe already reports and fail a run
// before anything is deployed. They live beside the planner because they read
// the workers it kept.

package discovery

import (
	"fmt"
	"slices"
)

// noteStorageMTU records a Fail finding for each worker whose storage
// interfaces carry an MTU other than the fleet's.
//
// The fleet's MTU is the most common one, the larger on a tie, since a mixed
// fleet is usually one where jumbo frames were meant and missed somewhere. An
// MTU the probe did not read is not evidence of a mismatch.
func (p *Plan) noteStorageMTU() {
	counts := map[int]int{}
	readings := make([]map[string]int, len(p.Workers))
	for i, worker := range p.Workers {
		readings[i] = storageMTUs(worker)
		for _, mtu := range readings[i] {
			counts[mtu]++
		}
	}
	if len(counts) < 2 {
		return
	}
	fleet := 0
	for mtu, n := range counts {
		if n > counts[fleet] || (n == counts[fleet] && mtu > fleet) {
			fleet = mtu
		}
	}

	for i, worker := range p.Workers {
		names := make([]string, 0, len(readings[i]))
		for name := range readings[i] {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			mtu := readings[i][name]
			if mtu == fleet {
				continue
			}
			p.Findings = append(p.Findings, Finding{
				Node:     worker.Name,
				Severity: SeverityFail,
				Reason:   StorageMTUMismatch,
				Message: fmt.Sprintf("%s carries MTU %d on %s, and the rest of the storage network carries %d",
					worker.Name, mtu, name, fleet),
				Remedy: fmt.Sprintf("set the MTU of %s on %s to %d, or the others to %d", name, worker.Name, fleet, mtu),
			})
		}
	}
}

// storageMTUs is the MTU of each interface a worker carries storage traffic
// on: its data interfaces, or its management interface when it has none.
func storageMTUs(worker Worker) map[string]int {
	names := worker.Data
	if len(names) == 0 && worker.Mgmt.Name != "" {
		names = []string{worker.Mgmt.Name}
	}
	mtus := map[string]int{}
	for _, iface := range worker.Report.Interfaces {
		if iface.MTU > 0 && slices.Contains(names, iface.Name) {
			mtus[iface.Name] = iface.MTU
		}
	}
	return mtus
}
