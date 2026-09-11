// Round trips that start at the hub, which is the direction storage now takes.
//
// v1alpha1 is the storage version while v1alpha2 is served beside it
// (design-property-renames.md §3.8), so a controller writing v1alpha2 has its
// object converted down to v1alpha1 to be stored and back up to v1alpha2 on the
// next read. That makes hub → spoke → hub the fidelity that matters in practice,
// and it is not the same property as the spoke → hub → spoke trip the per-kind
// tests already cover: a field only the hub can express survives one and not the
// other.

package v1alpha1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestControlPlaneRoundTripsFromTheHub(t *testing.T) {
	checked := metav1.Now()

	hub := &v1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock", Namespace: "sb"},
		Spec: v1alpha2.ControlPlaneSpec{
			Source: &v1alpha2.ControlPlaneSource{
				Managed: &v1alpha2.ManagedControlPlane{Image: testImage},
			},
		},
		Status: v1alpha2.ControlPlaneStatus{
			Phase:       "Available",
			Message:     "healthy",
			LastChecked: &checked,
		},
	}

	var stored ControlPlane
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.ControlPlane
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if diff := cmp.Diff(hub, &back); diff != "" {
		t.Errorf("storing and reading back changed the object (-written +read):\n%s", diff)
	}
}

// A managed block with no image normalizes to an absent source, and that is the
// intended behavior rather than an accident worth stashing.
//
// v1alpha1 states the image as one optional string, so it cannot express "a
// source block was present but empty" — the information does not exist in the
// stored shape. Since an empty managed block selects nothing and configures
// nothing, dropping it loses no meaning, and the alternative would be an
// annotation carrying the fact that a user wrote two empty braces.
func TestControlPlaneEmptyManagedBlockNormalizesAway(t *testing.T) {
	hub := &v1alpha2.ControlPlane{
		Spec: v1alpha2.ControlPlaneSpec{
			Source: &v1alpha2.ControlPlaneSource{Managed: &v1alpha2.ManagedControlPlane{}},
		},
	}

	var stored ControlPlane
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.ControlPlane
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if back.Spec.Source != nil {
		t.Errorf("spec.source = %+v, want nil", back.Spec.Source)
	}
}

func TestStorageBackupRoundTripsFromTheHub(t *testing.T) {
	created := metav1.Now()

	hub := &v1alpha2.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "sb"},
		Spec: v1alpha2.StorageBackupSpec{
			ClusterRef:        "production",
			PVCRef:            &v1alpha2.PersistentVolumeClaimRef{Name: "claim-1", Namespace: "apps"},
			SnapshotName:      "snap-1",
			SourceClusterUUID: "11111111-2222-3333-4444-555555555555",
		},
		Status: v1alpha2.StorageBackupStatus{
			Phase:        v1alpha2.BackupPhaseDone,
			Message:      "completed",
			ClusterUUID:  "cluster-uuid",
			PVName:       "pv-1",
			PoolName:     "pool-1",
			LvolID:       "lvol-uuid",
			FSType:       "ext4",
			SnapshotID:   "snapshot-uuid",
			BackupID:     "backup-uuid",
			S3ID:         42,
			Size:         1 << 30,
			AllowedHosts: []map[string]string{{"host": "a"}},
			CreatedAt:    &created,
		},
	}

	var stored StorageBackup
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StorageBackup
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if diff := cmp.Diff(hub, &back); diff != "" {
		t.Errorf("storing and reading back changed the object (-written +read):\n%s", diff)
	}
}

func TestStorageClusterOpsRoundTripsFromTheHub(t *testing.T) {
	started := metav1.Now()

	hub := &v1alpha2.StorageClusterOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "sb"},
		Spec: v1alpha2.StorageClusterOpsSpec{
			ClusterRef:     "production",
			Action:         v1alpha2.StorageClusterOpsActionRollingRestart,
			RollingRestart: &v1alpha2.RollingRestartSpec{RefreshSNodeAPI: true},
		},
		Status: v1alpha2.StorageClusterOpsStatus{
			Phase:     v1alpha2.StorageClusterOpsPhaseRunning,
			Triggered: true,
			Message:   "restarting node-b",
			StartedAt: &started,
			RollingRestart: &v1alpha2.RollingRestartStatus{
				PendingNodes:   []string{"node-b"},
				ProcessedNodes: []string{"node-a"},
				NodePhase:      "restarting",
				PhaseTriggered: true,
			},
		},
	}

	var stored StorageClusterOps
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StorageClusterOps
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if diff := cmp.Diff(hub, &back); diff != "" {
		t.Errorf("storing and reading back changed the object (-written +read):\n%s", diff)
	}
}

func TestStorageNodeOpsRoundTripsFromTheHub(t *testing.T) {
	started := metav1.Now()
	filter := testSystemVolumeFilter
	force := true

	hub := &v1alpha2.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "sb"},
		Spec: v1alpha2.StorageNodeOpsSpec{
			NodeRef: "node-1",
			Action:  v1alpha2.StorageNodeOpsActionMigrate,
			Force:   &force,
			Migrate: &v1alpha2.MigrateSpec{
				TargetWorkerNode: "worker-5",
				NewSsdPcie:       []string{"0000:5e:00.0"},
			},
			Remove: &v1alpha2.RemoveSpec{SystemVolumeFilterRegex: &filter},
		},
		Status: v1alpha2.StorageNodeOpsStatus{
			Phase:           v1alpha2.StorageNodeOpsPhaseRunning,
			SubPhase:        v1alpha2.StorageNodeOpsSubPhaseRestarting,
			Message:         "waiting for node-1",
			VolumesMigrated: 7,
			VolumesPending:  3,
			Triggered:       true,
			StartedAt:       &started,
		},
	}

	var stored StorageNodeOps
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StorageNodeOps
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if diff := cmp.Diff(hub, &back); diff != "" {
		t.Errorf("storing and reading back changed the object (-written +read):\n%s", diff)
	}
}

// The pool's round trip is the widest, because its spec is the one the redesign
// regrouped: three top-level fields and a QoS block gather into spec.limits, a
// parameters struct and a toggle gather into spec.volumeDefaults, and the four
// ceilings change type on the way. Every one of those has to survive being
// stored as v1alpha1 and read back.
func TestStoragePoolRoundTripsFromTheHub(t *testing.T) {
	hub := &v1alpha2.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-a", Namespace: "simplyblock"},
		Spec: v1alpha2.StoragePoolSpec{
			ClusterRef:   "production",
			AllowedNodes: []string{"production-7f3a9c", "production-2b81de"},
			Limits: &v1alpha2.PoolLimits{
				Capacity:      "10T",
				MaxVolumeSize: "2T",
				IOPS:          ptr.To(int32(200000)),
				Throughput: &v1alpha2.ThroughputLimits{
					Read: ptr.To(int32(2048)), Write: ptr.To(int32(1024)), ReadWrite: ptr.To(int32(4096)),
				},
			},
			VolumeDefaults: &v1alpha2.VolumeDefaults{
				IOPS: ptr.To(int32(20000)),
				Throughput: &v1alpha2.ThroughputLimits{
					Read: ptr.To(int32(300)), Write: ptr.To(int32(200)), ReadWrite: ptr.To(int32(512)),
				},
				Filesystem:                "xfs",
				EnableCompression:         ptr.To(true),
				EnableEncryption:          ptr.To(true),
				EnableReplication:         ptr.To(false),
				EnableDHCHAP:              ptr.To(true),
				PriorityClass:             "high",
				Fabric:                    "tcp",
				MaxNamespacesPerSubsystem: ptr.To(int32(4)),
				Tune2fsReservedBlocks:     "1",
			},
		},
		Status: v1alpha2.StoragePoolStatus{
			Phase:                   v1alpha2.StoragePoolPhaseReady,
			UUID:                    "4f2c8a11-6b3d-4e19-9a55-0c7e1d8f2b34",
			Status:                  "online",
			StorageClassNames:       []string{"archive-ext4", "fast-xfs"},
			DefaultStorageClassName: "simplyblock-production",
			Limits: &v1alpha2.PoolLimitsStatus{
				Host: "node-a",
				IOPS: ptr.To(int32(200000)),
				Throughput: &v1alpha2.ThroughputLimits{
					Read: ptr.To(int32(2048)), Write: ptr.To(int32(1024)), ReadWrite: ptr.To(int32(4096)),
				},
			},
			AllowedNodes:       []string{"production-7f3a9c"},
			ActiveOpsRef:       "rebalance-1",
			Message:            "ready",
			ObservedGeneration: 3,
		},
	}

	var stored StoragePool
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StoragePool
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if diff := cmp.Diff(hub, &back); diff != "" {
		t.Errorf("storing and reading back changed the object (-written +read):\n%s", diff)
	}
}

// A pool that asked for no ceiling and no defaults comes back with neither group
// rather than with two empty ones, which for spec.volumeDefaults is the
// difference between a field that can still be set and one that never can.
func TestStoragePoolEmptyGroupsRoundTripAsAbsent(t *testing.T) {
	hub := &v1alpha2.StoragePool{Spec: v1alpha2.StoragePoolSpec{ClusterRef: "production"}}

	var stored StoragePool
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StoragePool
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if back.Spec.Limits != nil {
		t.Errorf("spec.limits = %+v, want nil", back.Spec.Limits)
	}
	if back.Spec.VolumeDefaults != nil {
		t.Errorf("spec.volumeDefaults = %+v, want nil", back.Spec.VolumeDefaults)
	}
}
