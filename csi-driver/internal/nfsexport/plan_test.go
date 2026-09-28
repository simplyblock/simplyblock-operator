package nfsexport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/export"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
	"github.com/simplyblock/atlas/volstack/plans"
)

// exportSpec is one export with everything the planner needs. No cluster is
// reachable in a unit test, so every case here falls through to the record.
func exportSpec() export.Spec {
	return export.Spec{
		VolumeUUID: "11111111-2222-3333-4444-555555555555",
		ClusterID:  "cluster-1",
		PoolID:     "pool-1",
		Path:       "/var/lib/simplyblock/exports/pvc-1",
		FSID:       "fsid-1",
		Clients:    []string{"10.0.0.0/24"},
	}
}

// recordedPlanner is a planner whose store already holds the stack record a
// previous assembly wrote, which is what a teardown reads when the control
// plane is unreachable.
func recordedPlanner(t *testing.T, spec export.Spec, params layers.FabricParams) planner {
	t.Helper()
	store := volstack.NewStore(t.TempDir())
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("encoding the fabric parameters: %v", err)
	}
	record := volstack.Record{
		Version:      volstack.RecordVersion,
		VolumeHandle: spec.StackHandle(),
		Plan: []volstack.Entry{
			{Layer: layerFabric, Params: raw},
			{Layer: "filesystem"},
		},
	}
	if err := store.Write(record); err != nil {
		t.Fatalf("writing the record: %v", err)
	}
	return planner{seams: plans.NodeConfig{}, store: store}
}

func TestPlanIsFabricThenFilesystem(t *testing.T) {
	spec := exportSpec()
	p := recordedPlanner(t, spec, layers.FabricParams{NQN: "nqn.subsystem", NSID: 1})

	plan, err := p.Plan(context.Background(), spec)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// The same two layers the block path stages, so an MDS host and a client
	// node treat one namespace the same way.
	if got := strings.Join(plan.Names(), ","); got != "fabric,filesystem" {
		t.Errorf("plan = %q, want fabric,filesystem", got)
	}
}

func TestPlanFormatsXFSWithThePinnedOptions(t *testing.T) {
	spec := exportSpec()
	p := recordedPlanner(t, spec, layers.FabricParams{NQN: "nqn.subsystem"})

	plan, err := p.Plan(context.Background(), spec)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// XFS is not a default here: no other Linux filesystem serves a SCSI
	// layout. The options are pinned because this image's mkfs.xfs defaults
	// features an older host kernel cannot mount (format.go).
	params, ok := plan[1].(volstack.Recorder).Params().(layers.FilesystemParams)
	if !ok {
		t.Fatalf("the top layer recorded %T, want FilesystemParams", plan[1].(volstack.Recorder).Params())
	}
	if params.FsType != export.FSType {
		t.Errorf("fsType = %q, want %q", params.FsType, export.FSType)
	}
	if len(exportFormatOptions()) == 0 {
		t.Error("the plan passes no format options, so mkfs.xfs would use this image's own defaults")
	}
}

func TestPlanFallsBackToTheStackRecord(t *testing.T) {
	spec := exportSpec()
	p := recordedPlanner(t, spec, layers.FabricParams{NQN: "nqn.subsystem", NSID: 7})

	// A finalizer has to converge when the control plane does not answer, and
	// the record names the namespace this host attached.
	plan, err := p.Plan(context.Background(), spec)
	if err != nil {
		t.Fatalf("Plan without a reachable control plane: %v", err)
	}

	params, ok := plan[0].(volstack.Recorder).Params().(layers.FabricParams)
	if !ok {
		t.Fatalf("the bottom layer recorded %T, want FabricParams", plan[0].(volstack.Recorder).Params())
	}
	if params.NQN != "nqn.subsystem" || params.NSID != 7 {
		t.Errorf("fabric = %+v, want the recorded subsystem and namespace", params)
	}
}

func TestPlanRefusesAVolumeNothingIdentifies(t *testing.T) {
	spec := exportSpec()
	// No record, and no reachable control plane: releasing a namespace nothing
	// identifies would detach whichever one was found first.
	p := planner{seams: plans.NodeConfig{}, store: volstack.NewStore(t.TempDir())}

	_, err := p.Plan(context.Background(), spec)
	if err == nil {
		t.Fatal("Plan built a stack for a namespace nothing identifies")
	}
	if !strings.Contains(err.Error(), spec.VolumeUUID) {
		t.Errorf("error = %v, want it to name the volume", err)
	}
}

func TestPlanRefusesASpecWithNoCluster(t *testing.T) {
	spec := exportSpec()
	spec.ClusterID = ""
	p := planner{seams: plans.NodeConfig{}, store: volstack.NewStore(t.TempDir())}

	if _, err := p.Plan(context.Background(), spec); err == nil {
		t.Fatal("Plan accepted a spec naming no cluster")
	}
}

func TestStackHandleCannotCollideWithAStagedVolume(t *testing.T) {
	spec := exportSpec()

	// Both record into /var/run/simplyblock/stacks, and a node plugin records
	// under the CSI volume handle.
	if spec.StackHandle() == spec.VolumeUUID || spec.StackHandle() == spec.FSID {
		t.Errorf("handle %q is one a staged volume could also be keyed by", spec.StackHandle())
	}
}
