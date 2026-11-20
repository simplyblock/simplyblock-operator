// What a fleet's probe reports say the workers' operating system is, as the one
// statement a document can carry.
//
// One document becomes one StorageCluster, which runs one storage-node
// DaemonSet, which carries one UBUNTU_HOST for every worker it schedules. There
// is nowhere for a per-worker answer to go, so this is a question about the
// fleet rather than about a machine, and the only honest answer for a fleet
// whose workers disagree is none at all.
//
// Concluding nothing is therefore a result and not a failure. It reaches the
// reviewer as a note naming what each worker runs, which is the thing a person
// can act on: either the fleet is wrong, or the document's hostOS is theirs to
// state by hand.

package discovery

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/simplyblock/atlas/inventory"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// HostOSFor is the operating system the draft states, and the notes explaining
// it.
//
// It returns nil when the workers disagree and when nothing read an os-release,
// which are different findings with the same consequence: the document states
// no host OS, and the cluster it expands into falls back to the defaults every
// hand-written cluster has.
func HostOSFor(plan Plan) (*simplyblockv1alpha2.HostOSSpec, []string) {
	byDistro := map[string][]string{}
	for _, worker := range plan.Workers {
		byDistro[worker.Report.HostOS.Distro] = append(byDistro[worker.Report.HostOS.Distro], worker.Name)
	}

	if len(byDistro) == 0 {
		return nil, nil
	}
	if len(byDistro) > 1 {
		return nil, []string{fmt.Sprintf(
			"the workers do not run one operating system, so hostOS is unstated: %s. "+
				"One document builds one cluster and one storage-node DaemonSet, which "+
				"carries one host OS for every worker it schedules, so this is a fleet to "+
				"correct or a hostOS to state by hand",
			distroSplit(byDistro))}
	}

	distro := slices.Collect(maps.Keys(byDistro))[0]
	if distro == "" {
		return nil, []string{
			"hostOS is unstated: no worker's os-release could be read, which on a probe " +
				"is the host's root filesystem not having been mounted into it. The cluster " +
				"this expands into states no host OS either",
		}
	}

	os := &simplyblockv1alpha2.HostOSSpec{Distro: distro, Family: familyOf(plan, distro)}
	return os, []string{fmt.Sprintf(
		"hostOS is %s%s, read from every worker's os-release. It is what ubuntuHost is "+
			"decided by, and the one distribution that decides it is Ubuntu, whose NVMe-oF "+
			"modules are in a package the base install does not carry",
		distro, familyNote(os.Family))}
}

// familyOf is the family the probes reported, or the one the distro itself
// places, for a report written before the probe concluded families.
//
// The reports win where they have one, because a derivative's family is in its
// os-release ID_LIKE and only the probe read the file.
func familyOf(plan Plan, distro string) simplyblockv1alpha2.HostOSFamily {
	for _, worker := range plan.Workers {
		if family := worker.Report.HostOS.Family; family != "" {
			return simplyblockv1alpha2.HostOSFamily(family)
		}
	}
	return simplyblockv1alpha2.HostOSFamily(inventory.FamilyOf(inventory.Distro(distro)))
}

// familyNote is the parenthesis the note carries when there is a family to name.
func familyNote(family simplyblockv1alpha2.HostOSFamily) string {
	if family == "" {
		return ", of no packaging family"
	}
	return fmt.Sprintf(" of the %s family", family)
}

// distroSplit is which workers run what, ordered so that two runs over one fleet
// read the same.
func distroSplit(byDistro map[string][]string) string {
	distros := slices.Sorted(maps.Keys(byDistro))

	parts := make([]string, 0, len(distros))
	for _, distro := range distros {
		workers := slices.Clone(byDistro[distro])
		slices.Sort(workers)
		parts = append(parts, fmt.Sprintf("%s on %s", orUnread(distro), strings.Join(workers, ", ")))
	}
	return strings.Join(parts, "; ")
}

// orUnread names the worker whose os-release nothing read, so that a split
// between a distribution and a blank says which is which.
func orUnread(distro string) string {
	return cmp.Or(distro, "an unread os-release")
}
