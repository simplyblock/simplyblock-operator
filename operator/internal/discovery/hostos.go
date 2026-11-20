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

	byVersion := map[string][]string{}
	for _, worker := range plan.Workers {
		byVersion[worker.Report.HostOS.Version] = append(byVersion[worker.Report.HostOS.Version], worker.Name)
	}

	os := &simplyblockv1alpha2.HostOSSpec{Distro: distro, Family: familyOf(plan, distro)}
	if len(byVersion) == 1 {
		os.Version = slices.Collect(maps.Keys(byVersion))[0]
	}
	notes := []string{fmt.Sprintf(
		"hostOS is %s%s%s, read from every worker's os-release. It is what ubuntuHost is "+
			"decided by, and the one distribution that decides it is Ubuntu, whose NVMe-oF "+
			"modules are in a package the base install does not carry",
		distro, versionNote(os.Version), familyNote(os.Family))}
	if len(byVersion) > 1 {
		notes = append(notes, fmt.Sprintf(
			"the workers run more than one release of %s, so hostOS states no version: %s",
			distro, workersBy(byVersion, "no version")))
	}
	return os, notes
}

// versionNote is the version the note names after the distro, when one is stated.
func versionNote(version string) string {
	if version == "" {
		return ""
	}
	return " " + version
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
// read the same. A worker whose os-release nothing read is named as such, so that
// a split between a distribution and a blank says which is which.
func distroSplit(byDistro map[string][]string) string {
	return workersBy(byDistro, "an unread os-release")
}

// workersBy is which workers read which value, ordered so that two runs over one
// fleet read the same, naming an empty value as blank.
func workersBy(byValue map[string][]string, blank string) string {
	values := slices.Sorted(maps.Keys(byValue))

	parts := make([]string, 0, len(values))
	for _, value := range values {
		workers := slices.Clone(byValue[value])
		slices.Sort(workers)
		parts = append(parts, fmt.Sprintf("%s on %s", cmp.Or(value, blank), strings.Join(workers, ", ")))
	}
	return strings.Join(parts, "; ")
}
