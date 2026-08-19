package volumemigration

import (
	"context"
	"fmt"
)

// HostHasSubsystem reports whether this host holds an NVMe connection to the
// subsystem nqn, by reading sysfs under sysRoot (empty for the default /sys).
//
// It is used to decide whether a node needs the migration's new target paths at all:
// the operator picks nodes from the consumers of the subsystem's volumes, and a node
// whose consumer disappeared between that decision and the Job starting has nothing
// left to validate.
//
// A lookup that cannot be trusted is an error rather than "absent", because the two
// are not equally safe: reporting a connected host as unconnected would let the
// migration cut over without switching that host's paths, which is the outage this
// check exists to prevent. An empty subsystem list is treated the same way — on a node
// that consumes one of these volumes, "no NVMe subsystems at all" means sysfs is not
// visible (a missing host mount), not that the volume is gone.
func HostHasSubsystem(ctx context.Context, sysRoot, nqn string) (bool, error) {
	if nqn == "" {
		return false, fmt.Errorf("host subsystem lookup: empty NQN")
	}
	subs, err := resolver(sysRoot).List(ctx)
	if err != nil {
		return false, fmt.Errorf("list host NVMe subsystems: %w", err)
	}
	if len(subs) == 0 {
		return false, fmt.Errorf(
			"host reports no NVMe subsystems at all (sysRoot %q): sysfs is not visible here, "+
				"so the absence of %s cannot be trusted", sysRootOrDefault(sysRoot), nqn)
	}
	for _, s := range subs {
		if s.NQN == nqn {
			return true, nil
		}
	}
	return false, nil
}

func sysRootOrDefault(sysRoot string) string {
	if sysRoot == "" {
		return "/sys"
	}
	return sysRoot
}
