// Tests for the StorageCluster conversion between v1alpha1 and the v1alpha2
// hub.
//
// Three kinds of row need proving, and they fail differently:
//
//   - A rename converts by assignment, and a wrong one is visible as a missing
//     value. The round trip catches those wholesale.
//   - A regrouping has to leave its parent absent when the source is absent,
//     because spec.kms is immutable once set and an empty block written by the
//     conversion could never be corrected.
//   - An inverting toggle has nowhere to leave evidence at all
//     (design-property-renames.md §3.4): migrationEnabled and disableMigration
//     are both booleans, so a conversion that copied instead of negating turns
//     the rebalancer's dry run on for every cluster that asked for the
//     opposite. Those get their own assertions rather than a diff.

package v1alpha1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestStorageClusterConvertToRenamesAndRegroups(t *testing.T) {
	src := &StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "sb"},
		Spec: StorageClusterSpec{
			MaxSubsystemCount:      ptr.To(int32(20)),
			VCPUCount:              ptr.To(int32(8)),
			MaxHugePagesSize:       "100G",
			HashicorpVaultSettings: &HashicorpVaultSettings{BaseURL: "https://vault.example.com:8200"},
			Backup: &BackupSpec{
				LocalEndpoint:        "https://s3.example.com",
				CredentialsSecretRef: BackupCredentialsSecretRef{Name: "backup-credentials"},
			},
		},
	}

	var dst v1alpha2.StorageCluster
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if got := dst.Spec.MinHugePagesSize; got != "100G" {
		t.Errorf("spec.minHugePagesSize = %q, want %q", got, "100G")
	}
	if dst.Spec.KMS == nil || dst.Spec.KMS.Vault == nil {
		t.Fatalf("spec.kms = %+v, want the vault the cluster named", dst.Spec.KMS)
	}
	if got := dst.Spec.KMS.Vault.Endpoint; got != "https://vault.example.com:8200" {
		t.Errorf("spec.kms.vault.endpoint = %q", got)
	}
	if dst.Spec.Backup == nil {
		t.Fatal("spec.backup is absent")
	}
	if got := dst.Spec.Backup.Endpoint; got != "https://s3.example.com" {
		t.Errorf("spec.backup.endpoint = %q", got)
	}
	if got := dst.Spec.Backup.CredentialsSecretRef.Name; got != "backup-credentials" {
		t.Errorf("spec.backup.credentialsSecretRef.name = %q", got)
	}
}

// An absent key store must stay absent rather than becoming an empty block.
// spec.kms is immutable once set, so a cluster handed an empty one by the
// conversion could never be given a real one.
func TestStorageClusterConvertToLeavesAnAbsentKMSAbsent(t *testing.T) {
	for name, vault := range map[string]*HashicorpVaultSettings{
		"unset": nil,
		"empty": {BaseURL: ""},
	} {
		t.Run(name, func(t *testing.T) {
			src := &StorageCluster{Spec: StorageClusterSpec{HashicorpVaultSettings: vault}}

			var dst v1alpha2.StorageCluster
			if err := src.ConvertTo(&dst); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			if dst.Spec.KMS != nil {
				t.Errorf("spec.kms = %+v, want nil", dst.Spec.KMS)
			}
		})
	}
}

// The inverting row of design-property-renames.md §2.3. Migration is on unless
// somebody turns it off under both spellings, so an unstated value has to stay
// unstated and a stated one has to flip.
func TestStorageClusterMigrationToggleInverts(t *testing.T) {
	for name, tc := range map[string]struct {
		migrationEnabled *bool
		wantDisable      *bool
	}{
		"unstated stays unstated":      {nil, nil},
		"enabled becomes not disabled": {ptr.To(true), ptr.To(false)},
		"disabled becomes disabled":    {ptr.To(false), ptr.To(true)},
	} {
		t.Run(name, func(t *testing.T) {
			src := &StorageCluster{Spec: StorageClusterSpec{
				VolumeAutoPlacement: &VolumeAutoPlacementSettings{
					MigrationEnabled: tc.migrationEnabled,
				},
			}}

			var dst v1alpha2.StorageCluster
			if err := src.ConvertTo(&dst); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			if diff := cmp.Diff(tc.wantDisable, dst.Spec.VolumeAutoPlacement.DisableMigration); diff != "" {
				t.Errorf("disableMigration (-want +got):\n%s", diff)
			}

			var back StorageCluster
			if err := back.ConvertFrom(&dst); err != nil {
				t.Fatalf("ConvertFrom: %v", err)
			}
			if diff := cmp.Diff(tc.migrationEnabled, back.Spec.VolumeAutoPlacement.MigrationEnabled); diff != "" {
				t.Errorf("migrationEnabled after the round trip (-want +got):\n%s", diff)
			}
		})
	}
}

// The two switches that move up out of the blocks they governed. An unstated
// one stays unstated, because writing a value here would state a default the
// controller owns.
func TestStorageClusterSwitchesMoveUpAndBackDown(t *testing.T) {
	src := &StorageCluster{Spec: StorageClusterSpec{
		VolumeMigrationSettings: &VolumeMigrationSettings{
			DataRealignment: &DataRealignmentSettings{Enabled: ptr.To(true)},
		},
		VolumeAutoPlacement: &VolumeAutoPlacementSettings{Enabled: ptr.To(true)},
	}}

	var dst v1alpha2.StorageCluster
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if e := dst.Spec.EnableDataRealignment; e == nil || !*e {
		t.Errorf("spec.enableDataRealignment = %v, want true", e)
	}
	if e := dst.Spec.EnableVolumeAutoPlacement; e == nil || !*e {
		t.Errorf("spec.enableVolumeAutoPlacement = %v, want true", e)
	}

	var back StorageCluster
	if err := back.ConvertFrom(&dst); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if e := back.Spec.VolumeMigrationSettings.DataRealignment.Enabled; e == nil || !*e {
		t.Errorf("volumeMigrationSettings.dataRealignment.enabled = %v, want true", e)
	}
	if e := back.Spec.VolumeAutoPlacement.Enabled; e == nil || !*e {
		t.Errorf("volumeAutoPlacement.enabled = %v, want true", e)
	}
}

// A cluster that stated neither switch nor block must not acquire one. The
// conversion writes an empty parent nowhere, so a spec that asked for nothing
// comes back asking for nothing.
func TestStorageClusterAnEmptySpecGainsNoBlocks(t *testing.T) {
	src := &StorageCluster{Spec: StorageClusterSpec{
		MaxSubsystemCount: ptr.To(int32(20)),
		VCPUCount:         ptr.To(int32(8)),
	}}

	var dst v1alpha2.StorageCluster
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	for name, block := range map[string]any{
		"kms":                     dst.Spec.KMS,
		"backup":                  dst.Spec.Backup,
		"volumeMigrationSettings": dst.Spec.VolumeMigrationSettings,
		"volumeAutoPlacement":     dst.Spec.VolumeAutoPlacement,
		"stripe":                  dst.Spec.Stripe,
		// spec.enableDataRealignment is deliberately not in this list: it is
		// written for a cluster that stated nothing, which is what keeps the
		// registered default's behavior across the upgrade.
	} {
		if !isAbsent(block) {
			t.Errorf("spec.%s = %+v, want nil", name, block)
		}
	}
}

// The metrics backend recases (design-property-renames.md §2.5). It is one
// field under both spellings, so a wrong mapping silently selects a different
// source of truth for every placement decision.
func TestStorageClusterMetricsBackendConvertsBothWays(t *testing.T) {
	assertEnumConvertsBothWays(t,
		[]enumPair{
			{"controlplane", string(v1alpha2.MetricsBackendControlPlane)},
			{"prometheus", string(v1alpha2.MetricsBackendPrometheus)},
			{"uniform", string(v1alpha2.MetricsBackendUniform)},
		},
		func(t *testing.T, backend string) string {
			src := &StorageCluster{Spec: StorageClusterSpec{
				VolumeAutoPlacement: &VolumeAutoPlacementSettings{
					MetricsBackend: ptr.To(MetricsBackend(backend)),
				},
			}}
			var hub v1alpha2.StorageCluster
			if err := src.ConvertTo(&hub); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			return string(*hub.Spec.VolumeAutoPlacement.MetricsBackend)
		},
		func(t *testing.T, backend string) string {
			src := &v1alpha2.StorageCluster{Spec: v1alpha2.StorageClusterSpec{
				VolumeAutoPlacement: &v1alpha2.VolumeAutoPlacementSettings{
					MetricsBackend: ptr.To(v1alpha2.MetricsBackend(backend)),
				},
			}}
			var back StorageCluster
			if err := back.ConvertFrom(src); err != nil {
				t.Fatalf("ConvertFrom: %v", err)
			}
			return string(*back.Spec.VolumeAutoPlacement.MetricsBackend)
		},
	)
}

// The four removals of design-property-renames.md §2.3. Each has nowhere to go
// on the hub, so round-tripping a v1alpha1 object through the API server has to
// return what was applied rather than silently dropping it.
func TestStorageClusterRemovedFieldsSurviveTheHub(t *testing.T) {
	src := &StorageCluster{
		Spec: StorageClusterSpec{
			VolumeMigrationSettings: &VolumeMigrationSettings{Enabled: ptr.To(false)},
			Backup: &BackupSpec{
				LocalEndpoint:        "https://s3.example.com",
				CredentialsSecretRef: BackupCredentialsSecretRef{Name: "creds"},
				SnapshotBackups:      ptr.To(true),
				WithCompression:      ptr.To(false),
				LocalTesting:         ptr.To(true),
				SecondaryTarget:      ptr.To(int32(2)),
			},
		},
	}

	var hub v1alpha2.StorageCluster
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	var back StorageCluster
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	if diff := cmp.Diff(src.Spec.Backup, back.Spec.Backup); diff != "" {
		t.Errorf("spec.backup (-before +after):\n%s", diff)
	}
	if e := back.Spec.VolumeMigrationSettings.Enabled; e == nil || *e {
		t.Errorf("volumeMigrationSettings.enabled = %v, want false", e)
	}
}

// A threshold beyond what this version's int32 can hold is stashed whole
// rather than truncated. A truncated threshold is a number the cluster would
// then alarm at, which is worse than an absent one.
func TestStorageClusterAWideThresholdIsNotTruncated(t *testing.T) {
	hub := &v1alpha2.StorageCluster{Spec: v1alpha2.StorageClusterSpec{
		WarningThreshold: &v1alpha2.CapacityThresholdSpec{
			Capacity: ptr.To(int64(1) << 40),
		},
	}}

	var stored StorageCluster
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if c := stored.Spec.WarningThresholdSpec.Capacity; c != nil {
		t.Errorf("stored capacity = %d, want it dropped rather than truncated", *c)
	}

	var back v1alpha2.StorageCluster
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if diff := cmp.Diff(hub.Spec.WarningThreshold, back.Spec.WarningThreshold); diff != "" {
		t.Errorf("warningThreshold (-want +got):\n%s", diff)
	}
}

// An ordinary percentage fits and must cost no annotation, or every cluster in
// the fleet acquires metadata nobody wrote.
func TestStorageClusterANarrowThresholdIsNotStashed(t *testing.T) {
	hub := &v1alpha2.StorageCluster{Spec: v1alpha2.StorageClusterSpec{
		WarningThreshold: &v1alpha2.CapacityThresholdSpec{Capacity: ptr.To(int64(75))},
	}}

	var stored StorageCluster
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if _, ok := stored.Annotations[annoClusterWarnThreshold]; ok {
		t.Errorf("a threshold that fits was stashed: %v", stored.Annotations)
	}
	if c := stored.Spec.WarningThresholdSpec.Capacity; c == nil || *c != 75 {
		t.Errorf("stored capacity = %v, want 75", c)
	}
}

// status.subPhase was a string and status.step is an object
// (design-crd-model.md §9.5): the old value reads into step.state and leaves
// the deadline absent, so an operation in flight across the upgrade keeps
// running rather than expiring immediately.
//
// The value is this version's own lowercase spelling, because that is the only
// one a stored object holds. Which step it becomes is
// TestStorageClusterTheLegacySubPhaseIsNormalized; what this asserts is that
// no deadline is invented for it.
func TestStorageClusterSubPhaseReadsIntoTheStep(t *testing.T) {
	src := &StorageCluster{Status: StorageClusterStatus{SubPhase: "creating"}}

	var dst v1alpha2.StorageCluster
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if got := dst.Status.Step.State; got == "" {
		t.Error("status.step.state is empty, so the creation cannot resume")
	}
	if dst.Status.Step.Deadline != nil {
		t.Errorf("status.step.deadline = %v, want absent", dst.Status.Step.Deadline)
	}
}

func TestStorageClusterRoundTripsThroughTheHub(t *testing.T) {
	realigned := metav1.Now()

	obj := &StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "sb"},
		Spec: StorageClusterSpec{
			MaxSubsystemCount:           ptr.To(int32(20)),
			VCPUCount:                   ptr.To(int32(8)),
			MaxHugePagesSize:            "100G",
			StripeSpec:                  &StripeSpec{DataChunks: ptr.To(int32(2)), ParityChunks: ptr.To(int32(1))},
			FabricType:                  "tcp",
			ClientDataIfname:            "eth1",
			NvmfBasePort:                ptr.To(int32(4420)),
			RpcBasePort:                 ptr.To(int32(8080)),
			SnodeApiPort:                ptr.To(int32(50001)),
			EnableFailureDomains:        ptr.To(true),
			EnableNodeAffinity:          ptr.To(true),
			EnableChecksumValidation:    ptr.To(true),
			EnableAtomic4kWrites:        ptr.To(true),
			HashicorpVaultSettings:      &HashicorpVaultSettings{BaseURL: "https://vault.example.com:8200"},
			WarningThresholdSpec:        &CapacityThresholdSpec{Capacity: ptr.To(int32(75)), ProvisionedCapacity: ptr.To(int32(150))},
			CriticalThresholdSpec:       &CapacityThresholdSpec{Capacity: ptr.To(int32(90)), ProvisionedCapacity: ptr.To(int32(200))},
			MaxConcurrentWorkerRestarts: ptr.To(int32(2)),
			Backup: &BackupSpec{
				LocalEndpoint:        "https://s3.example.com",
				CredentialsSecretRef: BackupCredentialsSecretRef{Name: "backup-credentials"},
			},
			VolumeMigrationSettings: &VolumeMigrationSettings{
				Enabled:         ptr.To(true),
				RebalancerImage: ptr.To("quay.io/simplyblock-io/rebalancer:26.2.2"),
				DataRealignment: &DataRealignmentSettings{
					Enabled:  ptr.To(true),
					Interval: &metav1.Duration{Duration: 10 * 60 * 1e9},
					MinMoves: ptr.To(int32(4)),
				},
			},
			VolumeAutoPlacement: &VolumeAutoPlacementSettings{
				Enabled:                 ptr.To(true),
				MigrationEnabled:        ptr.To(false),
				ImbalanceThreshold:      ptr.To(int32(80)),
				MetricsBackend:          ptr.To(MetricsBackendPrometheus),
				PrometheusURL:           ptr.To("http://prometheus.monitoring:9090"),
				LatencyBenchmarkEnabled: ptr.To(true),
				BaselineStrategy:        ptr.To(BaselineStrategyRollingWindow),
				BaselineColdStart:       ptr.To(BaselineColdStartPartialWindow),
				BaselineOutlierK:        ptr.To(3.0),
			},
		},
		Status: StorageClusterStatus{
			UUID:                        "8f3c1e70-9a2b-4d51-b1c7-2f6e0d9a4c88",
			SubPhase:                    "creating",
			ClusterName:                 "production",
			NQN:                         "nqn.2023-02.io.simplyblock:8f3c1e70",
			Status:                      "active",
			Rebalancing:                 ptr.To(false),
			VolumeMoveGeneration:        ptr.To(int64(412)),
			RealignedGeneration:         ptr.To(int64(412)),
			LastDataRealignmentAt:       &realigned,
			ErasureCodingScheme:         "2x1",
			Configured:                  true,
			MaxFaultTolerance:           ptr.To(int32(1)),
			MaxConcurrentWorkerRestarts: ptr.To(int32(1)),
			ActiveOpsRef:                "roll-the-fleet",
			RebalancingMetrics: &RebalancingMetrics{
				AvgDeviationPct: 12.5,
				MaxDeviationPct: 88.0,
				HottestNodeUUID: "node-a",
				CoolestNodeUUID: "node-b",
				NodeMetrics: []NodeLoadMetrics{
					{NodeUUID: "node-a", LatencyDeviationPct: 88.0, VolumeCount: 12, LastUpdated: realigned},
				},
			},
		},
	}

	var hub v1alpha2.StorageCluster
	if err := obj.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	var back StorageCluster
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	// The realignment note is expected and is not part of what was applied:
	// it records which side wrote the switch, which is the one thing the
	// stored shape cannot say on its own.
	delete(back.Annotations, annoClusterEnableRealign)
	if len(back.Annotations) == 0 {
		back.Annotations = nil
	}

	if diff := cmp.Diff(obj, &back); diff != "" {
		t.Errorf("round trip changed the object (-before +after):\n%s", diff)
	}
}

// The behavior test design-property-renames.md §3.4 asks for, and the one row
// of §2.3 whose default changes direction.
//
// What is asserted is the effective configuration rather than the field value:
// a cluster that had realignment on because that was this version's default
// must still have it on after the conversion, and the hub's enable-formed
// field is off when absent, so the conversion has to write it.
func TestStorageClusterRealignmentStaysOnForAClusterNobodyEdited(t *testing.T) {
	for name, settings := range map[string]*VolumeMigrationSettings{
		"no settings block at all":       nil,
		"a block with no realignment":    {RebalancerImage: ptr.To("rebalancer:v1")},
		"a realignment block with no on": {DataRealignment: &DataRealignmentSettings{MinMoves: ptr.To(int32(4))}},
	} {
		t.Run(name, func(t *testing.T) {
			src := &StorageCluster{Spec: StorageClusterSpec{VolumeMigrationSettings: settings}}

			var hub v1alpha2.StorageCluster
			if err := src.ConvertTo(&hub); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			if e := hub.Spec.EnableDataRealignment; e == nil || !*e {
				t.Errorf("spec.enableDataRealignment = %v, want realignment still on for a "+
					"cluster that never turned it off", e)
			}
		})
	}
}

// The other half of the same rule: a cluster that turned realignment off keeps
// it off, and one the hub deliberately left off does not have it turned on by
// a conversion applying this version's default to an object that is not this
// version's.
func TestStorageClusterRealignmentRespectsWhatWasStated(t *testing.T) {
	t.Run("v1alpha1 turned it off", func(t *testing.T) {
		src := &StorageCluster{Spec: StorageClusterSpec{
			VolumeMigrationSettings: &VolumeMigrationSettings{
				DataRealignment: &DataRealignmentSettings{Enabled: ptr.To(false)},
			},
		}}
		var hub v1alpha2.StorageCluster
		if err := src.ConvertTo(&hub); err != nil {
			t.Fatalf("ConvertTo: %v", err)
		}
		if e := hub.Spec.EnableDataRealignment; e == nil || *e {
			t.Errorf("spec.enableDataRealignment = %v, want false to be preserved", e)
		}
	})

	t.Run("the hub left it unstated", func(t *testing.T) {
		hub := &v1alpha2.StorageCluster{}

		var stored StorageCluster
		if err := stored.ConvertFrom(hub); err != nil {
			t.Fatalf("ConvertFrom: %v", err)
		}
		var back v1alpha2.StorageCluster
		if err := stored.ConvertTo(&back); err != nil {
			t.Fatalf("ConvertTo: %v", err)
		}
		if back.Spec.EnableDataRealignment != nil {
			t.Errorf("spec.enableDataRealignment = %v, want a hub object's own absence kept",
				*back.Spec.EnableDataRealignment)
		}
	})
}

// The two findings of the review on #536 that land in the conversion, each as
// the test that would have caught it. Both are the same mistake: a value this
// version spells differently, or does not carry at all, reaching the hub as
// something the hub cannot use.

// status.subPhase is lowercase here and the hub's step values are PascalCase,
// so copying it verbatim produces a step no graph declares and no CEL rule
// accepts. A cluster part-way through its creation when the upgrade runs would
// become unreadable at v1alpha2 rather than resuming.
func TestStorageClusterTheLegacySubPhaseIsNormalized(t *testing.T) {
	src := &StorageCluster{Status: StorageClusterStatus{SubPhase: "creating"}}

	var dst v1alpha2.StorageCluster
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	want := string(v1alpha2.StorageClusterStepCreating)
	if got := dst.Status.Step.State; got != want {
		t.Errorf("status.step.state = %q, want %q: the hub's steps are PascalCase and "+
			"this version's only value is not", got, want)
	}
}

// A step the hub does not declare is dropped rather than carried through. It
// can only come from a hand-edited object, and passing it on would make the
// object fail its own validation on the next write.
func TestStorageClusterAnUndeclaredSubPhaseIsDropped(t *testing.T) {
	src := &StorageCluster{Status: StorageClusterStatus{SubPhase: "teleporting"}}

	var dst v1alpha2.StorageCluster
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if got := dst.Status.Step.State; got != "" {
		t.Errorf("status.step.state = %q, want it dropped: no graph declares it", got)
	}
}

// spec.deviceClass does not exist in this version, and a CRD default is
// applied on a write rather than on a conversion, so an object still stored as
// v1alpha1 would read back with no class at all. The default describes the
// fleet that exists — NVMe is the only class the backend accepted before 26.4
// — so the conversion is where it has to be applied.
func TestStorageClusterTheDeviceClassDefaultsOnTheWayUp(t *testing.T) {
	src := &StorageCluster{Spec: StorageClusterSpec{
		MaxSubsystemCount: ptr.To(int32(20)),
		VCPUCount:         ptr.To(int32(8)),
	}}

	var dst v1alpha2.StorageCluster
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if got := dst.Spec.DeviceClass; got != v1alpha2.StorageClusterDeviceClassNVMe {
		t.Errorf("spec.deviceClass = %q, want NVMe for a cluster that predates the field", got)
	}
}

// A class the hub deliberately chose survives being stored and read back. The
// default must not overwrite it, which is the whole reason the value is
// stashed rather than recomputed.
func TestStorageClusterADeliberateDeviceClassSurvives(t *testing.T) {
	hub := &v1alpha2.StorageCluster{Spec: v1alpha2.StorageClusterSpec{
		MaxSubsystemCount: ptr.To(int32(20)),
		VCPUCount:         ptr.To(int32(8)),
		DeviceClass:       v1alpha2.StorageClusterDeviceClassLogicalBlock,
	}}

	var stored StorageCluster
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StorageCluster
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if got := back.Spec.DeviceClass; got != v1alpha2.StorageClusterDeviceClassLogicalBlock {
		t.Errorf("spec.deviceClass = %q, want the class the hub chose to survive", got)
	}
}

// A note about a removed field tracks that field, including when the field
// stops having a value.
//
// The stash family that carries the hub's own fields holds this already: an
// absent value clears the annotation rather than leaving the last one that was
// written. The family that carries this version's removed fields did not, and
// the two are read by the same conversion, so the pair disagreed about what an
// absent value means. Where the annotation survives a field that does not, the
// value comes back on the next read down and the object states something
// nobody wrote.
func TestStorageClusterARemovedFieldsNoteGoesWhenItsValueDoes(t *testing.T) {
	// An object carrying the notes, whose fields are all absent.
	src := &StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production",
			Namespace: "sb",
			Annotations: map[string]string{
				annoV1Alpha1SnapshotBackups:  "true",
				annoV1Alpha1WithCompression:  "true",
				annoV1Alpha1LocalTesting:     "true",
				annoV1Alpha1SecondaryTarget:  "2",
				annoV1Alpha1MigrationEnabled: "false",
			},
		},
		Spec: StorageClusterSpec{
			VolumeMigrationSettings: &VolumeMigrationSettings{},
			Backup: &BackupSpec{
				LocalEndpoint:        "https://s3.example.com",
				CredentialsSecretRef: BackupCredentialsSecretRef{Name: "backup-credentials"},
			},
		},
	}

	var hub v1alpha2.StorageCluster
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	for _, key := range []string{
		annoV1Alpha1SnapshotBackups, annoV1Alpha1WithCompression,
		annoV1Alpha1LocalTesting, annoV1Alpha1SecondaryTarget,
		annoV1Alpha1MigrationEnabled,
	} {
		if got, ok := hub.Annotations[key]; ok {
			t.Errorf("%s = %q, want it gone: the field it notes states nothing", key, got)
		}
	}

	// And nothing comes back on the way down.
	var back StorageCluster
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if b := back.Spec.Backup; b != nil {
		if b.SnapshotBackups != nil || b.WithCompression != nil ||
			b.LocalTesting != nil || b.SecondaryTarget != nil {
			t.Errorf("a backup field nobody set was restored from a stale note: %+v", b)
		}
	}
	if v := back.Spec.VolumeMigrationSettings; v != nil && v.Enabled != nil {
		t.Errorf("a migration toggle nobody set was restored from a stale note: %v", *v.Enabled)
	}
}

// The same rule with the parent gone rather than the field. A note about a
// block that no longer exists is a note about nothing, and leaving it writes
// the block back on the way down.
func TestStorageClusterARemovedFieldsNoteGoesWhenItsBlockDoes(t *testing.T) {
	src := &StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production",
			Namespace: "sb",
			Annotations: map[string]string{
				annoV1Alpha1SnapshotBackups:  "true",
				annoV1Alpha1SecondaryTarget:  "2",
				annoV1Alpha1MigrationEnabled: "false",
			},
		},
	}

	var hub v1alpha2.StorageCluster
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	for _, key := range []string{
		annoV1Alpha1SnapshotBackups, annoV1Alpha1SecondaryTarget, annoV1Alpha1MigrationEnabled,
	} {
		if got, ok := hub.Annotations[key]; ok {
			t.Errorf("%s = %q, want it gone: the block it belongs to is not there", key, got)
		}
	}
}

// The baseline enums were recased for v1alpha2 the way MetricsBackend was, so
// conversion has to map them rather than cast them. A cast would carry
// "rollingWindow" into a version whose Enum marker admits "RollingWindow," which
// the API server refuses on the next write — and the storage rewrite's unchanged
// write is exactly such a write.
func TestBaselineEnumsAreRecasedBothWays(t *testing.T) {
	for _, tc := range []struct {
		name          string
		strategy      BaselineStrategy
		coldStart     BaselineColdStartPolicy
		wantStrategy  v1alpha2.BaselineStrategy
		wantColdStart v1alpha2.BaselineColdStartPolicy
	}{
		{
			name:          "the defaults",
			strategy:      BaselineStrategyRollingWindow,
			coldStart:     BaselineColdStartPartialWindow,
			wantStrategy:  v1alpha2.BaselineStrategyRollingWindow,
			wantColdStart: v1alpha2.BaselineColdStartPartialWindow,
		},
		{
			name:          "the other member of each",
			strategy:      BaselineStrategyBenchmark,
			coldStart:     BaselineColdStartDefer,
			wantStrategy:  v1alpha2.BaselineStrategyBenchmark,
			wantColdStart: v1alpha2.BaselineColdStartDefer,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from := &VolumeAutoPlacementSettings{
				BaselineStrategy:  ptr.To(tc.strategy),
				BaselineColdStart: ptr.To(tc.coldStart),
			}

			hub := autoPlacementToHub(from)
			if got := *hub.BaselineStrategy; got != tc.wantStrategy {
				t.Errorf("baselineStrategy converted to %q, want %q", got, tc.wantStrategy)
			}
			if got := *hub.BaselineColdStart; got != tc.wantColdStart {
				t.Errorf("baselineColdStart converted to %q, want %q", got, tc.wantColdStart)
			}

			// And back, because a round trip that did not restore the spelling
			// would make the storage rewrite look like an edit.
			back := autoPlacementFromHub(hub)
			if got := *back.BaselineStrategy; got != tc.strategy {
				t.Errorf("baselineStrategy came back as %q, want the %q it went in as",
					got, tc.strategy)
			}
			if got := *back.BaselineColdStart; got != tc.coldStart {
				t.Errorf("baselineColdStart came back as %q, want the %q it went in as",
					got, tc.coldStart)
			}
		})
	}
}

// A value neither version declares passes through rather than being dropped, the
// same way MetricsBackend's does. A field that silently emptied itself would
// turn a typo into the default, and the schema is what refuses the typo.
func TestAnUndeclaredBaselineValuePassesThrough(t *testing.T) {
	hub := autoPlacementToHub(&VolumeAutoPlacementSettings{
		BaselineStrategy: ptr.To(BaselineStrategy("whatever")),
	})
	if got := string(*hub.BaselineStrategy); got != "whatever" {
		t.Errorf("an unrecognized value converted to %q, want it carried through", got)
	}
}
