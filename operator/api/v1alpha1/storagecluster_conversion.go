// Conversion of StorageCluster between this version and the v1alpha2 hub.
//
// This is the widest spec in the group, and what moves is
// design-storagecluster.md §12 together with the rows
// design-property-renames.md assigns to this kind:
//
//   - spec.maxHugePagesSize becomes spec.minHugePagesSize (§2.1). The name is
//     the only thing that changes: the field was already a floor rather than a
//     cap, so nothing about the value's meaning moves with it.
//   - spec.backup.localEndpoint becomes spec.backup.endpoint, and the store
//     gains a bucket, a prefix, and a region it never had (§2.1).
//   - spec.hashicorpVaultSettings.baseURL regroups under spec.kms.vault (§2.4).
//   - Three bare `enabled` toggles resolve: the realignment's and the
//     auto-placement's move up to spec.enableDataRealignment and
//     spec.enableVolumeAutoPlacement, and the migration's is removed (§2.3).
//   - volumeAutoPlacement.migrationEnabled inverts into disableMigration, and
//     latencyBenchmarkEnabled becomes enableLatencyBenchmark (§2.3).
//   - MetricsBackend's values recase (§2.5).
//   - The capacity thresholds widen from int32 to int64.
//
// **A regrouping allocates its parent only when it has something to put in it**
// (design-property-renames.md §3.3). An all-empty spec.kms or spec.backup
// written by the conversion is a value nobody authored, and spec.kms is
// immutable once set, so a cluster that arrived with an empty block could never
// be given a real one.
//
// **Nothing that cannot be represented on both sides is allowed to disappear**
// (design-api-upgrade.md §6.2), and the two directions take separate annotation
// keys because they are separate problems:
//
//   - A field this version has and the hub removed is stashed on the way *up*
//     under storage.simplyblock.io/v1alpha1-<field> and read back on the way
//     down. The four are volumeMigrationSettings.enabled and the three
//     backup fields describing how a copy is taken, which the control plane
//     keeps accepting and now defaults for itself.
//   - A field the hub has and this version cannot express is stashed on the way
//     *down* under storage.simplyblock.io/conversion-<field> and restored on
//     the way up. That is not an upgrade-time cost but a per-write one: while
//     v1alpha1 is the storage version, a field that does not survive is lost on
//     every write, and a controller writing status.phase would read back a
//     cluster that never had one.
//
// Both sets are conversion state and nothing else. Nothing but this file reads
// them, and they go when v1alpha1 does.

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// stashedTrue is how a boolean this version keeps and the hub removed is
// written down. Only true is ever recorded: each of these is a pointer on one
// side and a plain bool on the other, and an annotation for every false would
// appear on every cluster that set none of them.
const stashedTrue = "true"

// The annotations holding the four spec fields the hub removed. The value is
// the text a user wrote, so that round-tripping a v1alpha1 object through the
// API server returns what was applied.
const (
	annoV1Alpha1MigrationEnabled = "storage.simplyblock.io/v1alpha1-volumeMigrationSettings.enabled"
	annoV1Alpha1SnapshotBackups  = "storage.simplyblock.io/v1alpha1-backup.snapshotBackups"
	annoV1Alpha1WithCompression  = "storage.simplyblock.io/v1alpha1-backup.withCompression"
	annoV1Alpha1LocalTesting     = "storage.simplyblock.io/v1alpha1-backup.localTesting"
	annoV1Alpha1SecondaryTarget  = "storage.simplyblock.io/v1alpha1-backup.secondaryTarget"
)

// The annotations holding the hub fields this version cannot express. The key
// names the field's path in the hub, so that reading the metadata of a stored
// object says which field each value belongs to without consulting this file.
const (
	annoClusterBackupBucket = "storage.simplyblock.io/conversion-spec.backup.bucket"
	annoClusterBackupPrefix = "storage.simplyblock.io/conversion-spec.backup.prefix"
	annoClusterBackupRegion = "storage.simplyblock.io/conversion-spec.backup.region"
	annoClusterDeviceClass  = "storage.simplyblock.io/conversion-spec.deviceClass"

	// enableDataRealignment is stashed for a reason none of the others has,
	// and it is the one row of design-property-renames.md §2.3 whose default
	// changes direction. The registered field defaulted to on and the hub's
	// enable-formed one defaults to off, so an object nobody edited would lose
	// its realignment on upgrade, which §3.1 forbids. What separates "a
	// v1alpha1 object that never stated it" from "a v1alpha2 object that
	// deliberately left it off" is nothing in the stored shape, so the
	// conversion records it: the annotation is written on every trip down,
	// including for an absent value, and its absence on the way up is what
	// identifies an object a real v1alpha1 client wrote.
	annoClusterEnableRealign = "storage.simplyblock.io/conversion-spec.enableDataRealignment"

	annoClusterStatusPhase    = "storage.simplyblock.io/conversion-status.phase"
	annoClusterStatusStep     = "storage.simplyblock.io/conversion-status.step"
	annoClusterStatusTasks    = "storage.simplyblock.io/conversion-status.tasks"
	annoClusterStatusMessage  = "storage.simplyblock.io/conversion-status.message"
	annoClusterStatusObserved = "storage.simplyblock.io/conversion-status.observedGeneration"

	// The thresholds widen from int32 to int64 on the way up, so a hub value
	// beyond int32 has nowhere to go on the way down. It is stashed whole
	// rather than clamped: a clamped threshold is a number the cluster would
	// then alarm at, which is worse than an absent one.
	annoClusterWarnThreshold     = "storage.simplyblock.io/conversion-spec.warningThreshold"
	annoClusterCriticalThreshold = "storage.simplyblock.io/conversion-spec.criticalThreshold"
)

// creationStepToHub maps this version's creation sub-phase onto the hub's
// step. There is one value to map: `creating` is the only sub-phase the
// registered controller ever wrote, and the hub spells it `Creating`.
//
// A value in neither the table is dropped rather than passed through, which is
// the one place this file departs from the enum rule in
// controlplane_conversion.go. That rule holds where each version's own Enum
// marker rejects what it does not accept; here the target is a field of a
// shared type carrying a CEL rule instead, and a step no graph declares fails
// the machine's restore rather than the object's admission. An empty step
// restores to the graph's initial state, which is where a creation with an
// unrecognizable position should begin again.
var creationStepToHub = map[string]string{
	"creating": string(v1alpha2.StorageClusterStepCreating),
}

// creationStepFromHub is the inverse, derived so the two cannot disagree.
var creationStepFromHub = invertStringMap(creationStepToHub)

// metricsBackendToHub recases the rebalancer's metrics source
// (design-property-renames.md §2.5). Every value is listed, because the field
// is user-authored and appears in deployment manifests.
var metricsBackendToHub = map[string]string{
	"controlplane": string(v1alpha2.MetricsBackendControlPlane),
	"prometheus":   string(v1alpha2.MetricsBackendPrometheus),
	"uniform":      string(v1alpha2.MetricsBackendUniform),
}

// metricsBackendFromHub is the inverse, derived so the two cannot disagree
// about a value.
var metricsBackendFromHub = invertStringMap(metricsBackendToHub)

// ConvertTo converts this StorageCluster to the v1alpha2 hub.
func (src *StorageCluster) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha2.StorageCluster)

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()

	dst.Spec = v1alpha2.StorageClusterSpec{
		MaxSubsystemCount:           src.Spec.MaxSubsystemCount,
		VCPUCount:                   src.Spec.VCPUCount,
		MinHugePagesSize:            src.Spec.MaxHugePagesSize,
		Stripe:                      stripeToHub(src.Spec.StripeSpec),
		FabricType:                  src.Spec.FabricType,
		ClientDataIfname:            src.Spec.ClientDataIfname,
		NvmfBasePort:                src.Spec.NvmfBasePort,
		RpcBasePort:                 src.Spec.RpcBasePort,
		SnodeApiPort:                src.Spec.SnodeApiPort,
		EnableFailureDomains:        src.Spec.EnableFailureDomains,
		EnableNodeAffinity:          src.Spec.EnableNodeAffinity,
		EnableChecksumValidation:    src.Spec.EnableChecksumValidation,
		EnableAtomic4kWrites:        src.Spec.EnableAtomic4kWrites,
		KMS:                         kmsToHub(src.Spec.HashicorpVaultSettings),
		WarningThreshold:            thresholdToHub(src.Spec.WarningThresholdSpec),
		CriticalThreshold:           thresholdToHub(src.Spec.CriticalThresholdSpec),
		MaxConcurrentWorkerRestarts: src.Spec.MaxConcurrentWorkerRestarts,
		Backup:                      backupStoreToHub(src.Spec.Backup),
		VolumeMigrationSettings:     migrationSettingsToHub(src.Spec.VolumeMigrationSettings),
		VolumeAutoPlacement:         autoPlacementToHub(src.Spec.VolumeAutoPlacement),
	}

	// The two switches move up out of the blocks they governed, and they need
	// different treatment because their registered defaults differ.
	//
	// Auto-placement was off unless asked for, and the hub's enable-formed
	// field is too, so absent below stays absent above.
	if s := src.Spec.VolumeAutoPlacement; s != nil {
		dst.Spec.EnableVolumeAutoPlacement = s.Enabled
	}
	dst.Spec.EnableDataRealignment = realignmentToHub(&dst.ObjectMeta, src.Spec.VolumeMigrationSettings)

	// The removals of §2.3, kept as the text that was applied.
	if s := src.Spec.VolumeMigrationSettings; s != nil {
		stashRemovedBool(&dst.ObjectMeta, annoV1Alpha1MigrationEnabled, s.Enabled)
	}
	if b := src.Spec.Backup; b != nil {
		stashRemovedBool(&dst.ObjectMeta, annoV1Alpha1SnapshotBackups, b.SnapshotBackups)
		stashRemovedBool(&dst.ObjectMeta, annoV1Alpha1WithCompression, b.WithCompression)
		stashRemovedBool(&dst.ObjectMeta, annoV1Alpha1LocalTesting, b.LocalTesting)
		stashRemoved(&dst.ObjectMeta, annoV1Alpha1SecondaryTarget, formatInt32(b.SecondaryTarget))
	}

	dst.Status = v1alpha2.StorageClusterStatus{
		UUID:                        src.Status.UUID,
		ClusterName:                 src.Status.ClusterName,
		NQN:                         src.Status.NQN,
		ErasureCodingScheme:         src.Status.ErasureCodingScheme,
		Status:                      src.Status.Status,
		Configured:                  src.Status.Configured,
		Rebalancing:                 src.Status.Rebalancing,
		MaxFaultTolerance:           src.Status.MaxFaultTolerance,
		MaxConcurrentWorkerRestarts: src.Status.MaxConcurrentWorkerRestarts,
		VolumeMoveGeneration:        src.Status.VolumeMoveGeneration,
		RealignedGeneration:         src.Status.RealignedGeneration,
		LastDataRealignmentAt:       src.Status.LastDataRealignmentAt,
		ActiveOpsRef:                src.Status.ActiveOpsRef,
		RebalancingMetrics:          rebalancingMetricsToHub(src.Status.RebalancingMetrics),
	}

	// status.subPhase was a string and status.step is an object
	// (design-property-renames.md §2.7, design-crd-model.md §9.5): the old
	// value reads into step.state and leaves step.deadline absent, so an
	// operation in flight across the upgrade keeps running rather than
	// expiring immediately. The stash wins where there is one, because only it
	// can carry a deadline.
	//
	// It is mapped rather than copied. This version's one value is lowercase
	// and the hub's steps are PascalCase, so a verbatim copy would produce a
	// step no graph declares and the CEL rule on status.step rejects — a
	// cluster part-way through its creation when the upgrade ran would become
	// unreadable rather than resuming.
	if step, ok := creationStepToHub[src.Status.SubPhase]; ok {
		dst.Status.Step = statemachine.KubeSnapshot{State: step}
	}

	return restoreClusterHubOnly(&dst.ObjectMeta, dst)
}

// ConvertFrom converts the v1alpha2 hub into this StorageCluster.
func (dst *StorageCluster) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha2.StorageCluster)

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()

	dst.Spec = StorageClusterSpec{
		MaxSubsystemCount:           src.Spec.MaxSubsystemCount,
		VCPUCount:                   src.Spec.VCPUCount,
		MaxHugePagesSize:            src.Spec.MinHugePagesSize,
		StripeSpec:                  stripeFromHub(src.Spec.Stripe),
		FabricType:                  src.Spec.FabricType,
		ClientDataIfname:            src.Spec.ClientDataIfname,
		NvmfBasePort:                src.Spec.NvmfBasePort,
		RpcBasePort:                 src.Spec.RpcBasePort,
		SnodeApiPort:                src.Spec.SnodeApiPort,
		EnableFailureDomains:        src.Spec.EnableFailureDomains,
		EnableNodeAffinity:          src.Spec.EnableNodeAffinity,
		EnableChecksumValidation:    src.Spec.EnableChecksumValidation,
		EnableAtomic4kWrites:        src.Spec.EnableAtomic4kWrites,
		HashicorpVaultSettings:      kmsFromHub(src.Spec.KMS),
		WarningThresholdSpec:        thresholdFromHub(src.Spec.WarningThreshold),
		CriticalThresholdSpec:       thresholdFromHub(src.Spec.CriticalThreshold),
		MaxConcurrentWorkerRestarts: src.Spec.MaxConcurrentWorkerRestarts,
		Backup:                      backupStoreFromHub(src.Spec.Backup),
		VolumeMigrationSettings:     migrationSettingsFromHub(src.Spec.VolumeMigrationSettings),
		VolumeAutoPlacement:         autoPlacementFromHub(src.Spec.VolumeAutoPlacement),
	}

	// The two switches move back down into the blocks they came from. Each
	// allocates its parent only when there is something to put in it, so a
	// cluster that stated neither the toggle nor the block does not acquire an
	// empty one.
	//
	// The realignment's value is also recorded whole, absence included,
	// because the two versions default it differently and the stored shape
	// cannot otherwise say which of them wrote it.
	if err := stash(&dst.ObjectMeta, annoClusterEnableRealign,
		nullable(src.Spec.EnableDataRealignment)); err != nil {
		return err
	}
	if e := src.Spec.EnableDataRealignment; e != nil {
		if dst.Spec.VolumeMigrationSettings == nil {
			dst.Spec.VolumeMigrationSettings = &VolumeMigrationSettings{}
		}
		if dst.Spec.VolumeMigrationSettings.DataRealignment == nil {
			dst.Spec.VolumeMigrationSettings.DataRealignment = &DataRealignmentSettings{}
		}
		dst.Spec.VolumeMigrationSettings.DataRealignment.Enabled = e
	}
	if e := src.Spec.EnableVolumeAutoPlacement; e != nil {
		if dst.Spec.VolumeAutoPlacement == nil {
			dst.Spec.VolumeAutoPlacement = &VolumeAutoPlacementSettings{}
		}
		dst.Spec.VolumeAutoPlacement.Enabled = e
	}

	// The removals of §2.3 come back out of their annotations, each allocating
	// the block it belongs to only if the block is there or the value is.
	if v := unstashRemovedBool(&dst.ObjectMeta, annoV1Alpha1MigrationEnabled); v != nil {
		if dst.Spec.VolumeMigrationSettings == nil {
			dst.Spec.VolumeMigrationSettings = &VolumeMigrationSettings{}
		}
		dst.Spec.VolumeMigrationSettings.Enabled = v
	}
	snapshots := unstashRemovedBool(&dst.ObjectMeta, annoV1Alpha1SnapshotBackups)
	compression := unstashRemovedBool(&dst.ObjectMeta, annoV1Alpha1WithCompression)
	localTesting := unstashRemovedBool(&dst.ObjectMeta, annoV1Alpha1LocalTesting)
	secondary := parseInt32(unstashRemoved(&dst.ObjectMeta, annoV1Alpha1SecondaryTarget))
	if dst.Spec.Backup != nil {
		dst.Spec.Backup.SnapshotBackups = snapshots
		dst.Spec.Backup.WithCompression = compression
		dst.Spec.Backup.LocalTesting = localTesting
		dst.Spec.Backup.SecondaryTarget = secondary
	}

	dst.Status = StorageClusterStatus{
		UUID:                        src.Status.UUID,
		SubPhase:                    mapOrPassThrough(creationStepFromHub, src.Status.Step.State),
		ClusterName:                 src.Status.ClusterName,
		NQN:                         src.Status.NQN,
		Status:                      src.Status.Status,
		Rebalancing:                 src.Status.Rebalancing,
		VolumeMoveGeneration:        src.Status.VolumeMoveGeneration,
		RealignedGeneration:         src.Status.RealignedGeneration,
		LastDataRealignmentAt:       src.Status.LastDataRealignmentAt,
		ErasureCodingScheme:         src.Status.ErasureCodingScheme,
		Configured:                  src.Status.Configured,
		MaxFaultTolerance:           src.Status.MaxFaultTolerance,
		MaxConcurrentWorkerRestarts: src.Status.MaxConcurrentWorkerRestarts,
		ActiveOpsRef:                src.Status.ActiveOpsRef,
		RebalancingMetrics:          rebalancingMetricsFromHub(src.Status.RebalancingMetrics),
	}

	return stashClusterHubOnly(&dst.ObjectMeta, src)
}

// stashClusterHubOnly writes every hub field this version has nowhere to put.
//
// A field at its zero value writes no annotation, so a cluster that set none of
// them is not given metadata it never had, and an object stored before this
// conversion existed does not acquire a dozen keys on its first read.
func stashClusterHubOnly(meta *metav1.ObjectMeta, src *v1alpha2.StorageCluster) error {
	if b := src.Spec.Backup; b != nil {
		for _, field := range []struct {
			key   string
			value any
		}{
			{annoClusterBackupBucket, b.Bucket},
			{annoClusterBackupPrefix, b.Prefix},
			{annoClusterBackupRegion, b.Region},
		} {
			if err := stash(meta, field.key, field.value); err != nil {
				return err
			}
		}
	} else {
		// The block itself is gone, so any value stashed from an earlier write
		// is stale: leaving it would restore a bucket the hub no longer names.
		clear(meta, annoClusterBackupBucket, annoClusterBackupPrefix, annoClusterBackupRegion)
	}

	// A threshold is stashed only when it does not fit the narrower type, so an
	// ordinary percentage costs no annotation and an out-of-range one is not
	// silently truncated.
	if err := stashWideThreshold(meta, annoClusterWarnThreshold, src.Spec.WarningThreshold); err != nil {
		return err
	}
	if err := stashWideThreshold(meta, annoClusterCriticalThreshold, src.Spec.CriticalThreshold); err != nil {
		return err
	}

	// status.subPhase carries the step's state verbatim, because it is a free
	// string. Only the deadline has nowhere to go, so a step without one costs
	// no annotation and every cluster that has ever been created is spared it.
	if src.Status.Step.Deadline != nil {
		if err := stash(meta, annoClusterStatusStep, src.Status.Step); err != nil {
			return err
		}
	} else {
		clear(meta, annoClusterStatusStep)
	}

	// Only a class that is not the default is recorded. An absent annotation
	// already reads as NVMe on the way up, so stashing NVMe would put a note
	// on every cluster that predates the field and say nothing.
	deviceClass := string(src.Spec.DeviceClass)
	if src.Spec.DeviceClass == v1alpha2.StorageClusterDeviceClassNVMe {
		deviceClass = ""
	}

	for _, field := range []struct {
		key   string
		value any
	}{
		{annoClusterDeviceClass, deviceClass},
		{annoClusterStatusPhase, string(src.Status.Phase)},
		{annoClusterStatusTasks, src.Status.Tasks},
		{annoClusterStatusMessage, src.Status.Message},
		{annoClusterStatusObserved, src.Status.ObservedGeneration},
	} {
		if err := stash(meta, field.key, field.value); err != nil {
			return err
		}
	}
	return nil
}

// restoreClusterHubOnly reads them back and removes the annotations, so an
// object converted up carries the fields rather than both the fields and the
// notes about them.
func restoreClusterHubOnly(meta *metav1.ObjectMeta, dst *v1alpha2.StorageCluster) error {
	var bucket, prefix, region string
	for _, field := range []struct {
		key    string
		target any
	}{
		{annoClusterBackupBucket, &bucket},
		{annoClusterBackupPrefix, &prefix},
		{annoClusterBackupRegion, &region},
	} {
		if err := unstash(meta, field.key, field.target); err != nil {
			return err
		}
	}
	if dst.Spec.Backup != nil {
		dst.Spec.Backup.Bucket = bucket
		dst.Spec.Backup.Prefix = prefix
		dst.Spec.Backup.Region = region
	}

	if err := restoreWideThreshold(meta, annoClusterWarnThreshold, &dst.Spec.WarningThreshold); err != nil {
		return err
	}
	if err := restoreWideThreshold(meta, annoClusterCriticalThreshold, &dst.Spec.CriticalThreshold); err != nil {
		return err
	}

	var deviceClass, phase string
	if err := unstash(meta, annoClusterDeviceClass, &deviceClass); err != nil {
		return err
	}
	if deviceClass == "" {
		// This version has no such field and a CRD default is applied on a
		// write rather than on a conversion, so an object still stored as
		// v1alpha1 would otherwise read back with no class at all. NVMe is the
		// only class the backend accepted before 26.4, so the default
		// describes the fleet that exists (§3.1). A class the hub chose
		// survives because it was stashed on the way down.
		deviceClass = string(v1alpha2.StorageClusterDeviceClassNVMe)
	}
	dst.Spec.DeviceClass = v1alpha2.StorageClusterDeviceClass(deviceClass)
	if err := unstash(meta, annoClusterStatusPhase, &phase); err != nil {
		return err
	}
	dst.Status.Phase = v1alpha2.StorageClusterPhase(phase)

	// The step stash is the authority where there is one, because only it
	// carries a deadline; ConvertTo has already read status.subPhase into the
	// state for an object a real v1alpha1 client wrote.
	var step statemachine.KubeSnapshot
	if err := unstash(meta, annoClusterStatusStep, &step); err != nil {
		return err
	}
	if step.State != "" || step.Deadline != nil {
		dst.Status.Step = step
	}

	for _, field := range []struct {
		key    string
		target any
	}{
		{annoClusterStatusTasks, &dst.Status.Tasks},
		{annoClusterStatusMessage, &dst.Status.Message},
		{annoClusterStatusObserved, &dst.Status.ObservedGeneration},
	} {
		if err := unstash(meta, field.key, field.target); err != nil {
			return err
		}
	}
	return nil
}

// stashWideThreshold records a threshold whose value does not fit this
// version's int32, and removes any note from an earlier write that did.
func stashWideThreshold(
	meta *metav1.ObjectMeta, key string, threshold *v1alpha2.CapacityThresholdSpec,
) error {
	if threshold == nil || (fitsInt32(threshold.Capacity) && fitsInt32(threshold.ProvisionedCapacity)) {
		clear(meta, key)
		return nil
	}
	return stash(meta, key, threshold)
}

// restoreWideThreshold puts a stashed wide threshold back, replacing whatever
// the narrowed round trip produced.
func restoreWideThreshold(
	meta *metav1.ObjectMeta, key string, target **v1alpha2.CapacityThresholdSpec,
) error {
	var wide *v1alpha2.CapacityThresholdSpec
	if err := unstash(meta, key, &wide); err != nil {
		return err
	}
	if wide != nil {
		*target = wide
	}
	return nil
}

// fitsInt32 reports whether a threshold component survives the narrowing. An
// absent value survives trivially.
func fitsInt32(v *int64) bool {
	const maxInt32, minInt32 = int64(1)<<31 - 1, -(int64(1) << 31)
	return v == nil || (*v >= minInt32 && *v <= maxInt32)
}

// realignmentToHub decides what spec.enableDataRealignment says for an object
// converting up, which is the one place in this migration where a default
// changes direction (design-property-renames.md §2.3, §3.4).
//
// Three cases, and only the third is interesting. An object the hub wrote
// carries its own value in an annotation and gets it back verbatim, absence
// included. An object a real v1alpha1 client wrote and that stated the nested
// field gets what it stated. An object a real v1alpha1 client wrote and that
// stated nothing had realignment on, because that was this version's default,
// so the field is written true rather than left absent: leaving it absent
// would turn realignment off on a cluster nobody edited.
func realignmentToHub(meta *metav1.ObjectMeta, settings *VolumeMigrationSettings) *bool {
	var recorded nullableBool
	if err := unstash(meta, annoClusterEnableRealign, &recorded); err == nil && recorded.Present {
		return recorded.Value
	}
	if settings != nil && settings.DataRealignment != nil && settings.DataRealignment.Enabled != nil {
		return settings.DataRealignment.Enabled
	}
	return ptr.To(true)
}

// nullableBool is an optional boolean that can be written down as absent, which
// a bare *bool cannot: stash writes nothing at all for a nil pointer, and
// nothing is what an object that never went through this conversion also has.
type nullableBool struct {
	// Present is false only for a value that was never recorded, which is what
	// the zero value of this type decodes to.
	Present bool  `json:"present"`
	Value   *bool `json:"value,omitempty"`
}

func nullable(v *bool) nullableBool { return nullableBool{Present: true, Value: v} }

func stripeToHub(s *StripeSpec) *v1alpha2.StripeSpec {
	if s == nil {
		return nil
	}
	return &v1alpha2.StripeSpec{DataChunks: s.DataChunks, ParityChunks: s.ParityChunks}
}

func stripeFromHub(s *v1alpha2.StripeSpec) *StripeSpec {
	if s == nil {
		return nil
	}
	return &StripeSpec{DataChunks: s.DataChunks, ParityChunks: s.ParityChunks}
}

// kmsToHub regroups the one key store this version names into the block the hub
// states every provider under. An unset or empty endpoint leaves the block
// absent rather than allocating an empty one, because spec.kms is immutable
// once set and an empty block could never be corrected.
func kmsToHub(v *HashicorpVaultSettings) *v1alpha2.KMSSpec {
	if v == nil || v.BaseURL == "" {
		return nil
	}
	return &v1alpha2.KMSSpec{Vault: &v1alpha2.VaultKMS{BaseURL: v.BaseURL}}
}

func kmsFromHub(k *v1alpha2.KMSSpec) *HashicorpVaultSettings {
	if k == nil || k.Vault == nil || k.Vault.BaseURL == "" {
		return nil
	}
	return &HashicorpVaultSettings{BaseURL: k.Vault.BaseURL}
}

// thresholdToHub widens a threshold's two components. The direction is free;
// the narrowing back is what needs the stash.
func thresholdToHub(t *CapacityThresholdSpec) *v1alpha2.CapacityThresholdSpec {
	if t == nil {
		return nil
	}
	return &v1alpha2.CapacityThresholdSpec{
		Capacity:            widenInt32(t.Capacity),
		ProvisionedCapacity: widenInt32(t.ProvisionedCapacity),
	}
}

func thresholdFromHub(t *v1alpha2.CapacityThresholdSpec) *CapacityThresholdSpec {
	if t == nil {
		return nil
	}
	return &CapacityThresholdSpec{
		Capacity:            narrowInt64(t.Capacity),
		ProvisionedCapacity: narrowInt64(t.ProvisionedCapacity),
	}
}

func widenInt32(v *int32) *int64 {
	if v == nil {
		return nil
	}
	return ptr.To(int64(*v))
}

// narrowInt64 drops a value the narrower type cannot hold rather than
// truncating it, and the caller has stashed such a value whole.
func narrowInt64(v *int64) *int32 {
	if !fitsInt32(v) || v == nil {
		return nil
	}
	return ptr.To(int32(*v))
}

// backupStoreToHub renames the endpoint and drops the three fields describing
// how a copy is taken, which the caller has stashed. The bucket the hub
// requires has no source here and is restored from its own annotation, so an
// object a real v1alpha1 client wrote arrives with an empty one — which is
// accurate, since the registered type had no bucket and nothing in the store
// could be located.
func backupStoreToHub(b *BackupSpec) *v1alpha2.BackupStoreSpec {
	if b == nil {
		return nil
	}
	return &v1alpha2.BackupStoreSpec{
		Endpoint:             b.LocalEndpoint,
		CredentialsSecretRef: corev1.LocalObjectReference{Name: b.CredentialsSecretRef.Name},
	}
}

func backupStoreFromHub(b *v1alpha2.BackupStoreSpec) *BackupSpec {
	if b == nil {
		return nil
	}
	return &BackupSpec{
		LocalEndpoint:        b.Endpoint,
		CredentialsSecretRef: BackupCredentialsSecretRef{Name: b.CredentialsSecretRef.Name},
	}
}

// migrationSettingsToHub drops the block's `enabled` switch, which the hub
// removed outright, and keeps what is left: how migration behaves rather than
// whether it happens. A block whose only content was the switch converts to
// nothing rather than to an empty struct.
func migrationSettingsToHub(s *VolumeMigrationSettings) *v1alpha2.VolumeMigrationSettings {
	if s == nil {
		return nil
	}
	out := v1alpha2.VolumeMigrationSettings{RebalancerImage: s.RebalancerImage}
	if r := s.DataRealignment; r != nil && (r.Interval != nil || r.MinMoves != nil) {
		out.DataRealignment = &v1alpha2.DataRealignmentSettings{
			Interval: r.Interval,
			MinMoves: r.MinMoves,
		}
	}
	if out.RebalancerImage == nil && out.DataRealignment == nil {
		return nil
	}
	return &out
}

func migrationSettingsFromHub(s *v1alpha2.VolumeMigrationSettings) *VolumeMigrationSettings {
	if s == nil {
		return nil
	}
	out := VolumeMigrationSettings{RebalancerImage: s.RebalancerImage}
	if r := s.DataRealignment; r != nil {
		out.DataRealignment = &DataRealignmentSettings{Interval: r.Interval, MinMoves: r.MinMoves}
	}
	return &out
}

// autoPlacementToHub drops the block's `enabled` switch, which moved up to the
// spec, inverts migrationEnabled into disableMigration, and renames
// latencyBenchmarkEnabled.
//
// The inversion is the row design-property-renames.md §3.4 singles out. An
// unstated migrationEnabled means migration is on, and an unstated
// disableMigration means the same, so nil converts to nil and only an explicit
// value is negated. Writing `false` for an unstated field would be authoring
// configuration, and writing `true` would turn the dry run on.
func autoPlacementToHub(s *VolumeAutoPlacementSettings) *v1alpha2.VolumeAutoPlacementSettings {
	if s == nil {
		return nil
	}
	out := v1alpha2.VolumeAutoPlacementSettings{
		DisableMigration:            negate(s.MigrationEnabled),
		EvaluationInterval:          s.EvaluationInterval,
		ImbalanceThreshold:          s.ImbalanceThreshold,
		MinHotColdDifferencePct:     s.MinHotColdDifferencePct,
		DefaultCoolDownSeconds:      s.DefaultCoolDownSeconds,
		MaxVolumeMigrationsPerCycle: s.MaxVolumeMigrationsPerCycle,
		StorageNodeCandidateCount:   s.StorageNodeCandidateCount,
		PrometheusURL:               s.PrometheusURL,
		EnableLatencyBenchmark:      s.LatencyBenchmarkEnabled,
		LatencyBenchmarkInterval:    s.LatencyBenchmarkInterval,
		BaselineWindow:              s.BaselineWindow,
		BaselineMinSamples:          s.BaselineMinSamples,
		BaselineOutlierK:            s.BaselineOutlierK,
		IOPSWeight:                  s.IOPSWeight,
		ThroughputWeight:            s.ThroughputWeight,
	}
	if b := s.MetricsBackend; b != nil {
		out.MetricsBackend = ptr.To(v1alpha2.MetricsBackend(
			mapOrPassThrough(metricsBackendToHub, string(*b))))
	}
	if b := s.BaselineStrategy; b != nil {
		out.BaselineStrategy = ptr.To(v1alpha2.BaselineStrategy(*b))
	}
	if c := s.BaselineColdStart; c != nil {
		out.BaselineColdStart = ptr.To(v1alpha2.BaselineColdStartPolicy(*c))
	}
	return &out
}

func autoPlacementFromHub(s *v1alpha2.VolumeAutoPlacementSettings) *VolumeAutoPlacementSettings {
	if s == nil {
		return nil
	}
	out := VolumeAutoPlacementSettings{
		MigrationEnabled:            negate(s.DisableMigration),
		EvaluationInterval:          s.EvaluationInterval,
		ImbalanceThreshold:          s.ImbalanceThreshold,
		MinHotColdDifferencePct:     s.MinHotColdDifferencePct,
		DefaultCoolDownSeconds:      s.DefaultCoolDownSeconds,
		MaxVolumeMigrationsPerCycle: s.MaxVolumeMigrationsPerCycle,
		StorageNodeCandidateCount:   s.StorageNodeCandidateCount,
		PrometheusURL:               s.PrometheusURL,
		LatencyBenchmarkEnabled:     s.EnableLatencyBenchmark,
		LatencyBenchmarkInterval:    s.LatencyBenchmarkInterval,
		BaselineWindow:              s.BaselineWindow,
		BaselineMinSamples:          s.BaselineMinSamples,
		BaselineOutlierK:            s.BaselineOutlierK,
		IOPSWeight:                  s.IOPSWeight,
		ThroughputWeight:            s.ThroughputWeight,
	}
	if b := s.MetricsBackend; b != nil {
		out.MetricsBackend = ptr.To(MetricsBackend(
			mapOrPassThrough(metricsBackendFromHub, string(*b))))
	}
	if b := s.BaselineStrategy; b != nil {
		out.BaselineStrategy = ptr.To(BaselineStrategy(*b))
	}
	if c := s.BaselineColdStart; c != nil {
		out.BaselineColdStart = ptr.To(BaselineColdStartPolicy(*c))
	}
	return &out
}

// negate flips an optional boolean and leaves an absent one absent. Absent is
// what carries the field's default, and inventing a value here would state the
// opposite of what the user left unsaid.
func negate(v *bool) *bool {
	if v == nil {
		return nil
	}
	return ptr.To(!*v)
}

func rebalancingMetricsToHub(m *RebalancingMetrics) *v1alpha2.RebalancingMetrics {
	if m == nil {
		return nil
	}
	out := v1alpha2.RebalancingMetrics{
		AvgDeviationPct:  m.AvgDeviationPct,
		MaxDeviationPct:  m.MaxDeviationPct,
		HottestNodeUUID:  m.HottestNodeUUID,
		CoolestNodeUUID:  m.CoolestNodeUUID,
		ImbalancePercent: m.ImbalancePercent,
		LastEvaluatedAt:  m.LastEvaluatedAt,
		LastMigrationAt:  m.LastMigrationAt,
	}
	for _, n := range m.NodeMetrics {
		out.NodeMetrics = append(out.NodeMetrics, v1alpha2.NodeLoadMetrics(n))
	}
	return &out
}

func rebalancingMetricsFromHub(m *v1alpha2.RebalancingMetrics) *RebalancingMetrics {
	if m == nil {
		return nil
	}
	out := RebalancingMetrics{
		AvgDeviationPct:  m.AvgDeviationPct,
		MaxDeviationPct:  m.MaxDeviationPct,
		HottestNodeUUID:  m.HottestNodeUUID,
		CoolestNodeUUID:  m.CoolestNodeUUID,
		ImbalancePercent: m.ImbalancePercent,
		LastEvaluatedAt:  m.LastEvaluatedAt,
		LastMigrationAt:  m.LastMigrationAt,
	}
	for _, n := range m.NodeMetrics {
		out.NodeMetrics = append(out.NodeMetrics, NodeLoadMetrics(n))
	}
	return &out
}

// stashRemovedBool records an optional boolean this version has and the hub
// does not. An absent value writes nothing, so a cluster that stated none of
// the removed fields is not given metadata it never had.
func stashRemovedBool(meta *metav1.ObjectMeta, key string, value *bool) {
	if value == nil {
		return
	}
	if *value {
		stashRemoved(meta, key, stashedTrue)
		return
	}
	stashRemoved(meta, key, "false")
}

// unstashRemovedBool takes one back out. A value that is neither spelling is
// read as absent rather than as false, for the reason unstash drops an
// annotation that does not decode: a hand-edited value should cost the field
// rather than the object.
func unstashRemovedBool(meta *metav1.ObjectMeta, key string) *bool {
	switch unstashRemoved(meta, key) {
	case stashedTrue:
		return ptr.To(true)
	case "false":
		return ptr.To(false)
	default:
		return nil
	}
}
