// How a drain sorts the volumes it finds on a node.
//
// The four buckets are what the whole of the Remove action is decided from:
// managed volumes are moved, system volumes are deleted, and a pinned or
// unmanaged one blocks the drain before it has suspended anything. Every one of
// the drain's five steps asks this question and two of them ask nothing else, so
// a volume placed in the wrong bucket is either data moved that somebody pinned
// where it is or data destroyed that nothing in Kubernetes was tracking.
//
// design-storagenode.md §8.1.

package node

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// onNode is one volume the control plane reports as living on the node being
// drained.
func onNode(uuid, name string) webapi.VolumeInfo {
	return webapi.VolumeInfo{UUID: uuid, Name: name, PrimaryNodeUUID: opsNodeID}
}

// censusOf classifies what the given control plane reports, through a drain of
// the node the fixtures describe.
func censusOf(
	t *testing.T, api *scriptedControlPlane, objects ...client.Object,
) volumeCensus {
	t.Helper()
	r, _ := anOpsWorld(t, api, objects...)
	census, err := r.classify(context.Background(),
		anOperation("a-drain", simplyblockv1alpha2.StorageNodeOpsActionRemove),
		opsClusterID, opsNodeID)
	if err != nil {
		t.Fatalf("classifying the node's volumes: %v", err)
	}
	return census
}

// A volume a PersistentVolume accounts for and nothing pins is movable, and it
// is carried with that PersistentVolume's name because the migration is
// addressed by the Kubernetes object rather than by the backend volume.
func TestAVolumeKubernetesAccountsForIsMovable(t *testing.T) {
	census := censusOf(t,
		aControlPlane().holding(onNode("volume-1", "pvc-abc")),
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))

	if len(census.Managed) != 1 {
		t.Fatalf("managed = %v, want the one volume a PersistentVolume accounts for", census.Managed)
	}
	if census.Managed[0].PVName != "pv-1" {
		t.Errorf("the movable volume names %q, and the move is addressed by the object",
			census.Managed[0].PVName)
	}
	if len(census.Pinned)+len(census.Unmanaged)+len(census.System) != 0 {
		t.Errorf("the volume landed in a second bucket as well: %+v", census)
	}
}

// A pinned claim blocks. Moving it would violate the pin, and the operator does
// not remove somebody's placement decision on their behalf.
func TestAPinnedVolumeBlocksRatherThanMoving(t *testing.T) {
	census := censusOf(t,
		aControlPlane().holding(onNode("volume-1", "pvc-abc")),
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", true))

	if len(census.Pinned) != 1 || census.Pinned[0] != "volume-1" {
		t.Errorf("pinned = %v, want the volume whose claim carries the annotation", census.Pinned)
	}
	if len(census.Managed) != 0 {
		t.Errorf("a pinned volume was also queued to move: %v", census.Managed)
	}
}

// A volume with no PersistentVolume behind it blocks, and blocking is the only
// safe answer: migrating it moves data nothing in Kubernetes tracks, and
// deleting it destroys data nothing in Kubernetes tracks.
func TestAVolumeNothingAccountsForBlocks(t *testing.T) {
	census := censusOf(t, aControlPlane().holding(onNode("volume-orphan", "hand-made")))

	if len(census.Unmanaged) != 1 || census.Unmanaged[0] != "volume-orphan" {
		t.Errorf("unmanaged = %v, want the volume no PersistentVolume accounts for", census.Unmanaged)
	}
}

// A PersistentVolume of another driver accounts for nothing here, so the volume
// behind it is as unaccounted for as one with no object at all.
func TestAnotherDriversVolumeAccountsForNothing(t *testing.T) {
	foreign := aPersistentVolume("pv-1", "volume-1")
	foreign.Spec.CSI.Driver = "ebs.csi.aws.com"

	census := censusOf(t,
		aControlPlane().holding(onNode("volume-1", "pvc-abc")),
		foreign, aClaim("pv-1", false))

	if len(census.Unmanaged) != 1 {
		t.Errorf("unmanaged = %v, want the volume a foreign driver's object does not account for",
			census.Unmanaged)
	}
}

// A PersistentVolume with no claim cannot be pinned, because the annotation
// lives on the claim. It is movable.
func TestAnUnclaimedVolumeIsMovable(t *testing.T) {
	unclaimed := aPersistentVolume("pv-1", "volume-1")
	unclaimed.Spec.ClaimRef = nil

	census := censusOf(t,
		aControlPlane().holding(onNode("volume-1", "pvc-abc")), unclaimed)

	if len(census.Managed) != 1 {
		t.Errorf("managed = %v, want the volume no claim can pin", census.Managed)
	}
}

// The rebalancer's benchmark volumes are per-node artifacts, so they are neither
// moved nor counted as blockers: verification deletes them, which is why the
// bucket carries the pool the delete is addressed by.
func TestABenchmarkVolumeIsTheDrainsToDelete(t *testing.T) {
	census := censusOf(t,
		aControlPlane().holding(onNode("volume-bench", "sb-fio-baseline-read")))

	if len(census.System) != 1 {
		t.Fatalf("system = %v, want the benchmark volume", census.System)
	}
	if census.System[0].PoolUUID != opsPool {
		t.Errorf("the benchmark volume carries pool %q, and a delete is addressed by both",
			census.System[0].PoolUUID)
	}
	if len(census.Managed)+len(census.Unmanaged) != 0 {
		t.Errorf("the benchmark volume was also treated as a user's: %+v", census)
	}
}

// A volume whose delete has been accepted is on its way out, and the backend
// keeps reporting it briefly. Counting it would hold a drain open against a
// volume that is already leaving.
func TestAVolumeAlreadyBeingDeletedIsNotCounted(t *testing.T) {
	leaving := onNode("volume-1", "pvc-abc")
	leaving.Status = volumeStatusInDeletion

	census := censusOf(t, aControlPlane().holding(leaving))

	if len(census.Managed)+len(census.Pinned)+len(census.Unmanaged)+len(census.System) != 0 {
		t.Errorf("a volume in deletion was counted: %+v", census)
	}
}

// Only the node being drained is the drain's business. A peer's volumes are in
// the same pools and must not be.
func TestAPeersVolumesAreNotThisNodesToMove(t *testing.T) {
	elsewhere := webapi.VolumeInfo{UUID: "volume-2", Name: "pvc-def", PrimaryNodeUUID: opsPeerID}

	census := censusOf(t, aControlPlane().holding(elsewhere))

	if len(census.Managed)+len(census.Unmanaged) != 0 {
		t.Errorf("another node's volume was counted: %+v", census)
	}
}

// A claim that cannot be read is counted as unmanaged for safety and recorded as
// incomplete, which is the flag that tells the caller this is a transient false
// positive rather than a volume nothing accounts for. An API server that is
// briefly away is not permission to move a volume nobody could classify.
func TestAClaimThatCannotBeReadMakesTheCensusIncomplete(t *testing.T) {
	r, _ := anOpsWorldWith(t, aControlPlane().holding(onNode("volume-1", "pvc-abc")),
		refusingClaims(), aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))

	census, err := r.classify(context.Background(),
		anOperation("a-drain", simplyblockv1alpha2.StorageNodeOpsActionRemove),
		opsClusterID, opsNodeID)
	if err != nil {
		t.Fatalf("classifying the node's volumes: %v", err)
	}

	if !census.Incomplete {
		t.Error("the census does not say it is incomplete, so the drain would act on a guess")
	}
	if len(census.Unmanaged) != 1 {
		t.Errorf("unmanaged = %v, want the unclassifiable volume counted where it blocks",
			census.Unmanaged)
	}
}

// A PersistentVolume of this driver whose handle names no volume indexes
// nothing, rather than indexing every such object under one empty key.
func TestAnEmptyVolumeHandleIndexesNothing(t *testing.T) {
	blank := aPersistentVolume("pv-1", "volume-1")
	blank.Spec.CSI.VolumeHandle = ""

	census := censusOf(t,
		aControlPlane().holding(onNode("volume-1", "pvc-abc")),
		blank, aClaim("pv-1", false))

	if len(census.Unmanaged) != 1 {
		t.Errorf("unmanaged = %v, want the volume an empty handle accounts for nothing about",
			census.Unmanaged)
	}
}

// The default pattern is the CRD's, and it matches what the rebalancer makes
// rather than what a user does.
func TestTheDefaultFilterMatchesTheBenchmarkVolumesOnly(t *testing.T) {
	filter, err := systemVolumeFilter(
		anOperation("a-drain", simplyblockv1alpha2.StorageNodeOpsActionRemove))
	if err != nil {
		t.Fatalf("compiling the default pattern: %v", err)
	}
	if !filter.MatchString("sb-fio-baseline-read") {
		t.Error("the default pattern does not match the rebalancer's own benchmark volume")
	}
	if filter.MatchString("pvc-a-users-volume") {
		t.Error("the default pattern matches a user's volume, which verification would delete")
	}
}

// An operation that states a pattern is asking for that one instead.
func TestAStatedFilterReplacesTheDefault(t *testing.T) {
	ops := anOperation("a-drain", simplyblockv1alpha2.StorageNodeOpsActionRemove)
	custom := "^bench-.*"
	ops.Spec.Remove = &simplyblockv1alpha2.RemoveSpec{SystemVolumeFilterRegex: &custom}

	filter, err := systemVolumeFilter(ops)
	if err != nil {
		t.Fatalf("compiling the stated pattern: %v", err)
	}
	if !filter.MatchString("bench-read") {
		t.Error("the stated pattern does not match what it says it matches")
	}
	if filter.MatchString("sb-fio-baseline-read") {
		t.Error("the default pattern is still being applied beside the stated one")
	}
}

// A pattern that does not compile is fatal rather than retried: the expression
// is in the spec, and no number of passes will make it parse.
func TestAPatternThatDoesNotCompileEndsTheOperation(t *testing.T) {
	ops := anOperation("a-drain", simplyblockv1alpha2.StorageNodeOpsActionRemove)
	broken := "["
	ops.Spec.Remove = &simplyblockv1alpha2.RemoveSpec{SystemVolumeFilterRegex: &broken}

	_, err := systemVolumeFilter(ops)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind: retrying cannot make a pattern parse", err)
	}
}
