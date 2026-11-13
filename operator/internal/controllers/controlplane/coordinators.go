// The coordinator choice of a HostMaintenance window: given the cluster file
// FoundationDB is running on and the process groups the FoundationDB operator
// reports, the set of coordinators that names no process on the host leaving.
//
// It is a pure function, kept apart from the steps that call it, because it is
// where the window's risk concentrates and a table is the right proof for it.
// The rules it implements are design-fdb-host-maintenance.md §5.5: keep the
// entries already off the host, fill the vacated ones one host at a time, then
// from hosts that already hold one, and write nothing when the list cannot be
// filled. A set it produces satisfies the FoundationDB operator's own validity
// rules, which is what keeps that operator from reverting it.

package controlplane

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// fdbProcessGroup is what the choice needs of one entry of
// FoundationDBCluster.status.processGroups.
type fdbProcessGroup struct {
	// ID is the process group ID, which is also the suffix of its pod's name.
	ID string
	// Class is the FoundationDB process class.
	Class string
	// FaultDomain is the zone, which is the worker's hostname label.
	FaultDomain string
	// Healthy is a group with no processGroupConditions and no removal
	// timestamp. The FoundationDB operator already distrusts the others.
	Healthy bool
	// Missing is a group carrying the MissingProcesses condition, which is a
	// process that has gone and not come back: what AwaitingHost waits on.
	Missing bool
}

// coordinatorInput is everything the choice reads.
type coordinatorInput struct {
	// ClusterFile is the current content of the cluster-file ConfigMap key.
	ClusterFile string
	// Groups are the process groups the FoundationDBCluster reports.
	Groups []fdbProcessGroup
	// Zone is the fault domain of the host this window takes roles off.
	Zone string
	// LeavingZones are the fault domains of other windows that have observed
	// their drain. Their processes are about to go and take no coordinator.
	LeavingZones map[string]bool
	// ClusterName, Namespace, and DNSDomain build a process group's address.
	ClusterName string
	Namespace   string
	DNSDomain   string
}

// coordinatorChoice is the set to write and how it was placed.
type coordinatorChoice struct {
	// Set is the whole coordinator list to write, nil when nothing is to be
	// written.
	Set []string
	// Placement is Spread, SharedHost, or Unchanged.
	Placement simplyblockv1alpha2.HostMaintenanceCoordinatorPlacement
	// Kept and Added are how many entries of the current list survived and how
	// many were filled.
	Kept  int
	Added int
	// Wanted and Available are the slots to fill and the candidates there were
	// for them, which is what an Unchanged placement reports.
	Wanted    int
	Available int
}

// errMixedPorts is a cluster file whose entries do not agree on a port. The
// window builds addresses from that port, so a file that carries two is one it
// cannot extend without guessing.
var errMixedPorts = errors.New("the cluster file names coordinators on different ports")

// classRank orders the stateful classes the way the choice takes them. A class
// absent from the map is not stateful and is never a candidate.
var classRank = map[string]int{
	"log":         0,
	"storage":     1,
	"transaction": 2,
	"coordinator": 3,
}

// chooseCoordinators is the choice of design §5.5.
func chooseCoordinators(in coordinatorInput) (coordinatorChoice, error) {
	unchanged := coordinatorChoice{Placement: simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementUnchanged}

	entries, port, err := parseClusterFile(in.ClusterFile)
	if err != nil {
		return unchanged, err
	}
	if len(entries) == 0 {
		return unchanged, nil
	}

	byHost := make(map[string]fdbProcessGroup, len(in.Groups))
	for _, g := range in.Groups {
		byHost[in.hostname(g.ID)] = g
	}

	// An entry the groups do not explain is kept as it is: its zone is unknown,
	// and a coordinator the window cannot place is not one it should drop.
	keep := make([]string, 0, len(entries))
	usedHosts := map[string]bool{}
	onZone := false
	for _, e := range entries {
		g, known := byHost[e.host]
		if known && g.FaultDomain == in.Zone {
			onZone = true
			continue
		}
		keep = append(keep, e.raw)
		if known {
			usedHosts[g.FaultDomain] = true
		}
	}
	if !onZone {
		return unchanged, nil
	}

	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	candidates := make([]fdbProcessGroup, 0, len(in.Groups))
	for _, g := range in.Groups {
		if _, stateful := classRank[g.Class]; !stateful || !g.Healthy {
			continue
		}
		if g.FaultDomain == in.Zone || in.LeavingZones[g.FaultDomain] {
			continue
		}
		if kept[in.address(g.ID, port)] {
			continue
		}
		candidates = append(candidates, g)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if classRank[candidates[i].Class] != classRank[candidates[j].Class] {
			return classRank[candidates[i].Class] < classRank[candidates[j].Class]
		}
		return candidates[i].ID < candidates[j].ID
	})

	want := len(entries) - len(keep)
	var spread, shared []fdbProcessGroup
	taken := map[string]bool{}
	for _, g := range candidates {
		if len(spread) == want {
			break
		}
		if usedHosts[g.FaultDomain] {
			continue
		}
		spread = append(spread, g)
		usedHosts[g.FaultDomain] = true
		taken[g.ID] = true
	}
	for _, g := range candidates {
		if len(spread)+len(shared) == want {
			break
		}
		if taken[g.ID] {
			continue
		}
		shared = append(shared, g)
	}

	if len(spread)+len(shared) < want {
		unchanged.Wanted = want
		unchanged.Available = len(spread) + len(shared)
		return unchanged, nil
	}

	set := make([]string, 0, len(entries))
	set = append(set, keep...)
	for _, g := range spread {
		set = append(set, in.address(g.ID, port))
	}
	for _, g := range shared {
		set = append(set, in.address(g.ID, port))
	}
	placement := simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementSpread
	if len(shared) > 0 {
		placement = simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementSharedHost
	}
	return coordinatorChoice{
		Set: set, Placement: placement,
		Kept: len(keep), Added: want, Wanted: want, Available: len(candidates),
	}, nil
}

// hostname is the DNS name the FoundationDB operator gives a process group's
// pod, which is what its cluster-file entry starts with.
func (in coordinatorInput) hostname(id string) string {
	return fmt.Sprintf("%s-%s.%s.%s.svc.%s", in.ClusterName, id, in.ClusterName, in.Namespace, in.DNSDomain)
}

// address is a process group's cluster-file entry on the port the file uses.
func (in coordinatorInput) address(id, port string) string {
	return in.hostname(id) + ":" + port
}

// clusterFileEntry is one coordinator of a cluster file: its host, and the
// entry as written so a kept one is written back unchanged.
type clusterFileEntry struct {
	host string
	raw  string
}

// parseClusterFile reads `<description>:<id>@<entry>,<entry>,...` and returns the
// entries and the one port they share. An entry is `<host>:<port>` with an
// optional `:tls` suffix.
func parseClusterFile(file string) ([]clusterFileEntry, string, error) {
	file = strings.TrimSpace(file)
	if file == "" {
		return nil, "", nil
	}
	_, list, found := strings.Cut(file, "@")
	if !found {
		return nil, "", fmt.Errorf("the cluster file %q carries no coordinator list", file)
	}
	var entries []clusterFileEntry
	port := ""
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		hostPort := strings.TrimSuffix(raw, ":tls")
		host, p, ok := strings.Cut(hostPort, ":")
		if !ok || host == "" || p == "" {
			return nil, "", fmt.Errorf("the cluster file entry %q is not host:port", raw)
		}
		if port != "" && p != port {
			return nil, "", fmt.Errorf("%w: %s and %s", errMixedPorts, port, p)
		}
		port = p
		entries = append(entries, clusterFileEntry{host: host, raw: raw})
	}
	return entries, port, nil
}
