// The coordinator choice of a HostMaintenance window, as a table.
//
// Every case is a cluster shape and a leaving host, and what is asserted is the
// set the window writes and how it is placed. The function reads nothing from
// the API, so the table is the whole of its proof; the rows are U-01 through
// U-16 and U-85 in test-plan-fdb-host-maintenance.md.

package controlplane

import (
	"errors"
	"slices"
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// vm11 is the host every case drains unless it says otherwise.
	vm11             = "vm11"
	testFDBCluster   = "simplyblock-fdb-cluster"
	testFDBNamespace = "simplyblock"
	testDNSDomain    = "cluster.local"
)

// addr is the cluster-file address of a process group, as the window writes it.
func addr(id string) string {
	return testFDBCluster + "-" + id + "." + testFDBCluster + "." + testFDBNamespace + ".svc." + testDNSDomain + ":4501"
}

func clusterFile(ids ...string) string {
	addrs := make([]string, 0, len(ids))
	for _, id := range ids {
		addrs = append(addrs, addr(id))
	}
	return "simplyblock_fdb_cluster:dKilY3sx@" + strings.Join(addrs, ",")
}

func group(id, class, host string) fdbProcessGroup {
	return fdbProcessGroup{ID: id, Class: class, FaultDomain: host, Healthy: true}
}

// fourHosts is the lab: a log and a storage process on vm11, vm12, and vm13,
// and a storage process and the cluster controller on vm14, with the three
// logs as coordinators.
func fourHosts() []fdbProcessGroup {
	return []fdbProcessGroup{
		group("log-1", "log", vm11), group("storage-1", "storage", vm11),
		group("log-2", "log", "vm12"), group("storage-2", "storage", "vm12"),
		group("log-3", "log", "vm13"),
		group("storage-4", "storage", "vm14"), group("cc-4", "cluster_controller", "vm14"),
	}
}

func choose(t *testing.T, in coordinatorInput) coordinatorChoice {
	t.Helper()
	in.ClusterName, in.Namespace, in.DNSDomain = testFDBCluster, testFDBNamespace, testDNSDomain
	choice, err := chooseCoordinators(in)
	if err != nil {
		t.Fatalf("choose: %v", err)
	}
	return choice
}

// U-01: four hosts, the leaving one holds a coordinator, a fourth host is free.
func TestTheVacatedSlotMovesToTheHostWithoutACoordinator(t *testing.T) {
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "log-2", "log-3"), Groups: fourHosts(), Zone: vm11,
	})
	want := []string{addr("log-2"), addr("log-3"), addr("storage-4")}
	if !slices.Equal(choice.Set, want) {
		t.Errorf("set = %v, want %v", choice.Set, want)
	}
	if choice.Placement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementSpread {
		t.Errorf("placement = %s, want Spread", choice.Placement)
	}
}

// U-02: nothing on the leaving host, so nothing is written.
func TestAHostWithoutACoordinatorChangesNothing(t *testing.T) {
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "log-2", "log-3"), Groups: fourHosts(), Zone: "vm14",
	})
	if choice.Set != nil || choice.Placement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementUnchanged {
		t.Errorf("set = %v placement = %s, want no set and Unchanged", choice.Set, choice.Placement)
	}
}

// U-03: the entries off the host are kept verbatim and in place.
func TestEntriesOffTheHostAreKeptVerbatim(t *testing.T) {
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-3", "log-1", "log-2"), Groups: fourHosts(), Zone: vm11,
	})
	if choice.Set[0] != addr("log-3") || choice.Set[1] != addr("log-2") {
		t.Errorf("set = %v, want log-3 and log-2 kept in their order before the new entry", choice.Set)
	}
	if choice.Kept != 2 || choice.Added != 1 {
		t.Errorf("kept %d added %d, want 2 and 1", choice.Kept, choice.Added)
	}
}

// U-04: candidates come in class order, then by process group ID.
func TestCandidatesAreTakenByClassThenByID(t *testing.T) {
	groups := []fdbProcessGroup{
		group("log-1", "log", vm11),
		group("storage-9", "storage", "vm12"), group("storage-2", "storage", "vm12"),
		group("log-3", "log", "vm13"), group("storage-3", "storage", "vm13"),
		group("storage-8", "storage", "vm14"), group("storage-4", "storage", "vm14"),
	}
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "storage-9", "log-3"), Groups: groups, Zone: vm11,
	})
	// vm12 and vm13 already hold one; vm14's lowest storage ID fills the slot.
	want := []string{addr("storage-9"), addr("log-3"), addr("storage-4")}
	if !slices.Equal(choice.Set, want) {
		t.Errorf("set = %v, want %v", choice.Set, want)
	}

	// With vm14 gone too, the shared slot goes to a log before a storage, and
	// the lowest ID within the class.
	groups = append(groups[:5:5], group("log-0", "log", "vm13"))
	choice = choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "storage-9", "log-3"), Groups: groups, Zone: vm11,
	})
	want = []string{addr("storage-9"), addr("log-3"), addr("log-0")}
	if !slices.Equal(choice.Set, want) {
		t.Errorf("set = %v, want %v", choice.Set, want)
	}
}

// U-05, U-06: an unhealthy or departing process group takes no coordinator.
func TestAnUnhealthyOrDepartingGroupIsNotACandidate(t *testing.T) {
	groups := fourHosts()
	for i := range groups {
		if groups[i].ID == "storage-4" {
			groups[i].Healthy = false
		}
	}
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "log-2", "log-3"), Groups: groups, Zone: vm11,
	})
	if slices.Contains(choice.Set, addr("storage-4")) {
		t.Errorf("set = %v, an unhealthy group was chosen", choice.Set)
	}
	if choice.Placement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementSharedHost {
		t.Errorf("placement = %s, want SharedHost once vm14 has no healthy candidate", choice.Placement)
	}
}

// U-07: only stateful classes are eligible.
func TestOnlyStatefulClassesAreCandidates(t *testing.T) {
	groups := []fdbProcessGroup{
		group("log-1", "log", vm11), group("log-2", "log", "vm12"), group("log-3", "log", "vm13"),
		group("cc-4", "cluster_controller", "vm14"), group("stateless-4", "stateless", "vm14"),
	}
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "log-2", "log-3"), Groups: groups, Zone: vm11,
	})
	for _, a := range choice.Set {
		if strings.Contains(a, "cc-4") || strings.Contains(a, "stateless-4") {
			t.Errorf("set = %v, a non-stateful class was chosen", choice.Set)
		}
	}
	if choice.Placement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementUnchanged {
		t.Errorf("placement = %s, want Unchanged with no stateful candidate off the host", choice.Placement)
	}
}

// U-08: three hosts, three coordinators, so the third shares a host.
func TestThreeHostsShareAHost(t *testing.T) {
	groups := []fdbProcessGroup{
		group("log-1", "log", vm11), group("storage-1", "storage", vm11),
		group("log-2", "log", "vm12"), group("storage-2", "storage", "vm12"),
		group("log-3", "log", "vm13"), group("storage-3", "storage", "vm13"),
	}
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "log-2", "log-3"), Groups: groups, Zone: vm11,
	})
	want := []string{addr("log-2"), addr("log-3"), addr("storage-2")}
	if !slices.Equal(choice.Set, want) {
		t.Errorf("set = %v, want %v", choice.Set, want)
	}
	if choice.Placement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementSharedHost {
		t.Errorf("placement = %s, want SharedHost", choice.Placement)
	}
}

// U-09: two hosts, the leaving one holds two coordinators.
func TestTwoHostsPutBothSlotsOnTheRemainingHost(t *testing.T) {
	groups := []fdbProcessGroup{
		group("log-1", "log", vm11), group("storage-1", "storage", vm11),
		group("log-2", "log", "vm12"), group("storage-2", "storage", "vm12"), group("storage-5", "storage", "vm12"),
	}
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "storage-1", "log-2"), Groups: groups, Zone: vm11,
	})
	want := []string{addr("log-2"), addr("storage-2"), addr("storage-5")}
	if !slices.Equal(choice.Set, want) {
		t.Errorf("set = %v, want %v", choice.Set, want)
	}
	if choice.Placement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementSharedHost {
		t.Errorf("placement = %s, want SharedHost", choice.Placement)
	}
}

// U-10: one host, nothing to choose from.
func TestOneHostCannotMoveAnything(t *testing.T) {
	groups := []fdbProcessGroup{group("log-1", "log", vm11), group("storage-1", "storage", vm11)}
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1"), Groups: groups, Zone: vm11,
	})
	if choice.Placement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementUnchanged || choice.Set != nil {
		t.Errorf("placement = %s set = %v, want Unchanged and no set", choice.Placement, choice.Set)
	}
	if choice.Wanted != 1 || choice.Available != 0 {
		t.Errorf("wanted %d available %d, want 1 and 0 reported", choice.Wanted, choice.Available)
	}
}

// U-11: five hosts, five coordinators, two on the leaving host.
func TestFiveCoordinatorsSpreadOverTheFreeHosts(t *testing.T) {
	groups := []fdbProcessGroup{
		group("log-1", "log", "h1"), group("storage-1", "storage", "h1"),
		group("log-2", "log", "h2"), group("log-3", "log", "h3"),
		group("storage-4", "storage", "h4"), group("storage-5", "storage", "h5"),
		group("storage-6", "storage", "h6"), group("storage-7", "storage", "h7"),
	}
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "storage-1", "log-2", "log-3", "storage-4"), Groups: groups, Zone: "h1",
	})
	want := []string{addr("log-2"), addr("log-3"), addr("storage-4"), addr("storage-5"), addr("storage-6")}
	if !slices.Equal(choice.Set, want) {
		t.Errorf("set = %v, want %v", choice.Set, want)
	}
	if choice.Placement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementSpread {
		t.Errorf("placement = %s, want Spread", choice.Placement)
	}
}

// U-12: fewer candidates than slots, so nothing is written.
func TestTooFewCandidatesWriteNothing(t *testing.T) {
	groups := []fdbProcessGroup{
		group("log-1", "log", vm11), group("storage-1", "storage", vm11),
		group("log-2", "log", "vm12"),
	}
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "storage-1", "log-2"), Groups: groups, Zone: vm11,
	})
	if choice.Set != nil || choice.Placement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementUnchanged {
		t.Errorf("set = %v placement = %s, want nothing written", choice.Set, choice.Placement)
	}
	if choice.Wanted != 2 || choice.Available != 0 {
		t.Errorf("wanted %d available %d, want 2 and 0", choice.Wanted, choice.Available)
	}
}

// U-13: the address is built from the cluster, the ID, the namespace, the DNS
// domain, and the port of the current file.
func TestTheAddressIsBuiltFromTheClusterFilePort(t *testing.T) {
	file := "simplyblock_fdb_cluster:x@" + strings.ReplaceAll(clusterFile("log-1", "log-2", "log-3")[len("simplyblock_fdb_cluster:dKilY3sx@"):], ":4501", ":4600")
	choice := choose(t, coordinatorInput{ClusterFile: file, Groups: fourHosts(), Zone: vm11})
	want := "simplyblock-fdb-cluster-storage-4.simplyblock-fdb-cluster.simplyblock.svc.cluster.local:4600"
	if !slices.Contains(choice.Set, want) {
		t.Errorf("set = %v, want it to carry %s", choice.Set, want)
	}
}

// U-14: entries on different ports are an error, not a guess.
func TestMixedPortsAreRefused(t *testing.T) {
	file := "simplyblock_fdb_cluster:x@" + addr("log-1") + "," + strings.Replace(addr("log-2"), ":4501", ":4502", 1) + "," + addr("log-3")
	_, err := chooseCoordinators(coordinatorInput{
		ClusterFile: file, Groups: fourHosts(), Zone: vm11,
		ClusterName: testFDBCluster, Namespace: testFDBNamespace, DNSDomain: testDNSDomain,
	})
	if err == nil {
		t.Fatal("a cluster file with two ports was accepted")
	}
	if !errors.Is(err, errMixedPorts) {
		t.Errorf("err = %v, want errMixedPorts", err)
	}
}

// U-15: the host is matched on the fault domain, so a group whose fault domain
// is the hostname label rather than the Node name is still the host's.
func TestTheHostIsMatchedOnTheFaultDomain(t *testing.T) {
	groups := fourHosts()
	for i := range groups {
		if groups[i].FaultDomain == vm11 {
			groups[i].FaultDomain = "vm11.simplyblock2.localdomain"
		}
	}
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "log-2", "log-3"), Groups: groups, Zone: "vm11.simplyblock2.localdomain",
	})
	if slices.Contains(choice.Set, addr("log-1")) {
		t.Errorf("set = %v, the leaving host's coordinator was kept", choice.Set)
	}
}

// U-16: empty inputs produce Unchanged and no panic.
func TestEmptyInputsChangeNothing(t *testing.T) {
	for name, in := range map[string]coordinatorInput{
		"no groups":  {ClusterFile: clusterFile("log-1", "log-2", "log-3"), Zone: vm11},
		"empty file": {ClusterFile: "", Groups: fourHosts(), Zone: vm11},
	} {
		t.Run(name, func(t *testing.T) {
			choice := choose(t, in)
			if choice.Set != nil || choice.Placement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementUnchanged {
				t.Errorf("set = %v placement = %s, want nothing written", choice.Set, choice.Placement)
			}
		})
	}
}

// U-85: a host whose window has observed its drain is not a target, and a
// cordoned host whose window has not is.
func TestALeavingHostIsNotATarget(t *testing.T) {
	choice := choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "log-2", "log-3"), Groups: fourHosts(), Zone: vm11,
		LeavingZones: map[string]bool{"vm14": true},
	})
	if slices.Contains(choice.Set, addr("storage-4")) {
		t.Errorf("set = %v, a host whose drain was observed was chosen", choice.Set)
	}
	if choice.Placement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementSharedHost {
		t.Errorf("placement = %s, want SharedHost with vm14 leaving", choice.Placement)
	}

	// A cordoned host that has not observed a drain is not in LeavingZones and
	// stays a target: that is the four-host case of U-01, repeated here so the
	// contrast is in one place.
	choice = choose(t, coordinatorInput{
		ClusterFile: clusterFile("log-1", "log-2", "log-3"), Groups: fourHosts(), Zone: vm11,
	})
	if !slices.Contains(choice.Set, addr("storage-4")) {
		t.Errorf("set = %v, a cordoned host with no drain observed was skipped", choice.Set)
	}
}
