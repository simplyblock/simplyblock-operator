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
	if got := dst.Spec.KMS.Vault.BaseURL; got != "https://vault.example.com:8200" {
		t.Errorf("spec.kms.vault.baseURL = %q", got)
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
func TestStorageClusterSubPhaseReadsIntoTheStep(t *testing.T) {
	src := &StorageCluster{Status: StorageClusterStatus{SubPhase: "Creating"}}

	var dst v1alpha2.StorageCluster
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if got := dst.Status.Step.State; got != "Creating" {
		t.Errorf("status.step.state = %q, want %q", got, "Creating")
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
			SubPhase:                    "Persisting",
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
