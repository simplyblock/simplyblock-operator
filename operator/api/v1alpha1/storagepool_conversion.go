// Conversion of StoragePool between this version and the v1alpha2 hub.
//
// This is the widest conversion in the migration, because the pool is the kind
// whose spec was regrouped rather than only renamed (design-storagepool.md §11,
// design-property-renames.md §2.1 and §2.4):
//
//   - spec.clusterName becomes spec.clusterRef.
//   - spec.capacityLimit, spec.logicalVolumeMaxSize, and spec.qos.* gather under
//     spec.limits, which is what the pool as a whole is held to.
//   - spec.storageClassParameters.* gathers under spec.volumeDefaults, which is
//     what each volume in the pool gets, and the four QoS ceilings stop being
//     strings on the way.
//   - spec.dhchap moves into that block as enableDHCHAP, and encryption becomes
//     enableEncryption.
//   - status.qos becomes status.limits.
//
// **A regrouping allocates its parent only when it has something to put in it**
// (design-property-renames.md §3.3). An all-empty spec.limits or
// spec.volumeDefaults written by the conversion is a value nobody authored, and
// for spec.volumeDefaults it is worse than noise: the field is immutable once
// set, so a pool that arrived with an empty block could never be given a real
// one.
//
// **Nothing that cannot be represented on both sides is allowed to disappear**
// (design-api-upgrade.md §6.2), and the two directions of that have separate
// annotation keys because they are separate problems:
//
//   - A field this version has and the hub removed is stashed on the way *up*
//     under storage.simplyblock.io/v1alpha1-<field> and read back on the way
//     down. spec.action and spec.status are the two, and what is preserved is
//     the text a user typed rather than any behavior: both were marked unused
//     here and neither ever had an effect.
//   - A field the hub has and this version cannot express is stashed on the way
//     *down* under storage.simplyblock.io/conversion-<field> and restored on the
//     way up. Without it, every one of them would be lost on each write for as
//     long as v1alpha1 is the storage version, which is not an upgrade-time cost
//     but a per-write one: a controller writing status.phase would read back a
//     pool that had never had a phase.
//
// Both sets are conversion state and nothing else. Nothing but this file reads
// them, and they go when v1alpha1 does.

package v1alpha1

import (
	"encoding/json"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/simplyblock/atlas/ptr"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The annotations holding the two spec fields the hub removed.
const (
	annoV1Alpha1PoolAction = "storage.simplyblock.io/v1alpha1-action"
	annoV1Alpha1PoolStatus = "storage.simplyblock.io/v1alpha1-status"
)

// The annotations holding the hub fields this version cannot express. The key
// names the field's path in the hub, so that reading the metadata of a stored
// object says which field each value belongs to without consulting this file.
const (
	annoEnableCompression       = "storage.simplyblock.io/conversion-spec.volumeDefaults.enableCompression"
	annoEnableReplication       = "storage.simplyblock.io/conversion-spec.volumeDefaults.enableReplication"
	annoPriorityClass           = "storage.simplyblock.io/conversion-spec.volumeDefaults.priorityClass"
	annoStatusPhase             = "storage.simplyblock.io/conversion-status.phase"
	annoStatusClassNames        = "storage.simplyblock.io/conversion-status.storageClassNames"
	annoStatusDefaultClassName  = "storage.simplyblock.io/conversion-status.defaultStorageClassName"
	annoStatusActiveOpsRef      = "storage.simplyblock.io/conversion-status.activeOpsRef"
	annoStatusMessage           = "storage.simplyblock.io/conversion-status.message"
	annoStatusObservedGeneraton = "storage.simplyblock.io/conversion-status.observedGeneration"
)

// ConvertTo converts this StoragePool to the v1alpha2 hub.
func (src *StoragePool) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha2.StoragePool)

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	stashRemoved(&dst.ObjectMeta, annoV1Alpha1PoolAction, src.Spec.Action)
	stashRemoved(&dst.ObjectMeta, annoV1Alpha1PoolStatus, src.Spec.Status)

	dst.Spec = v1alpha2.StoragePoolSpec{
		ClusterRef:     src.Spec.ClusterName,
		AllowedNodes:   src.Spec.AllowedNodes,
		Limits:         poolLimitsToHub(src.Spec),
		VolumeDefaults: volumeDefaultsToHub(src.Spec),
	}

	dst.Status = v1alpha2.StoragePoolStatus{
		UUID:         src.Status.UUID,
		Status:       src.Status.Status,
		Limits:       poolLimitsStatusToHub(src.Status.QoS),
		AllowedNodes: src.Status.AllowedNodes,
	}

	return restoreHubOnly(&dst.ObjectMeta, dst)
}

// ConvertFrom converts the v1alpha2 hub into this StoragePool.
func (dst *StoragePool) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha2.StoragePool)

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	dst.Spec = StoragePoolSpec{
		ClusterName:  src.Spec.ClusterRef,
		AllowedNodes: src.Spec.AllowedNodes,
		Action:       unstashRemoved(&dst.ObjectMeta, annoV1Alpha1PoolAction),
		Status:       unstashRemoved(&dst.ObjectMeta, annoV1Alpha1PoolStatus),
	}
	poolLimitsFromHub(&dst.Spec, src.Spec.Limits)
	volumeDefaultsFromHub(&dst.Spec, src.Spec.VolumeDefaults)

	dst.Status = StoragePoolStatus{
		UUID:         src.Status.UUID,
		Status:       src.Status.Status,
		QoS:          poolLimitsStatusFromHub(src.Status.Limits),
		AllowedNodes: src.Status.AllowedNodes,
	}

	return stashHubOnly(&dst.ObjectMeta, src)
}

// stashHubOnly writes every hub field this version has nowhere to put.
//
// A field at its zero value writes no annotation, so a pool that set none of
// them is not given metadata it never had, and an object stored before this
// conversion existed does not acquire nine keys on its first read.
func stashHubOnly(meta *metav1.ObjectMeta, src *v1alpha2.StoragePool) error {
	if d := src.Spec.VolumeDefaults; d != nil {
		if err := stash(meta, annoEnableCompression, d.EnableCompression); err != nil {
			return err
		}
		if err := stash(meta, annoEnableReplication, d.EnableReplication); err != nil {
			return err
		}
		if err := stash(meta, annoPriorityClass, d.PriorityClass); err != nil {
			return err
		}
	} else {
		// The block itself is gone, so any value stashed from an earlier write
		// is stale: leaving it would restore defaults the hub no longer states.
		clear(meta, annoEnableCompression, annoEnableReplication, annoPriorityClass)
	}

	status := src.Status
	for _, field := range []struct {
		key   string
		value any
	}{
		{annoStatusPhase, string(status.Phase)},
		{annoStatusClassNames, status.StorageClassNames},
		{annoStatusDefaultClassName, status.DefaultStorageClassName},
		{annoStatusActiveOpsRef, status.ActiveOpsRef},
		{annoStatusMessage, status.Message},
		{annoStatusObservedGeneraton, status.ObservedGeneration},
	} {
		if err := stash(meta, field.key, field.value); err != nil {
			return err
		}
	}
	return nil
}

// restoreHubOnly reads them back and removes the annotations, so an object
// converted up carries the fields rather than both the fields and the notes
// about them.
func restoreHubOnly(meta *metav1.ObjectMeta, dst *v1alpha2.StoragePool) error {
	var (
		compression *bool
		replication *bool
		priority    string
	)
	if err := unstash(meta, annoEnableCompression, &compression); err != nil {
		return err
	}
	if err := unstash(meta, annoEnableReplication, &replication); err != nil {
		return err
	}
	if err := unstash(meta, annoPriorityClass, &priority); err != nil {
		return err
	}
	if compression != nil || replication != nil || priority != "" {
		// The block is allocated only when something restored into it, for the
		// reason volumeDefaultsToHub returns nil rather than an empty struct.
		if dst.Spec.VolumeDefaults == nil {
			dst.Spec.VolumeDefaults = &v1alpha2.VolumeDefaults{}
		}
		dst.Spec.VolumeDefaults.EnableCompression = compression
		dst.Spec.VolumeDefaults.EnableReplication = replication
		dst.Spec.VolumeDefaults.PriorityClass = priority
	}

	var phase string
	if err := unstash(meta, annoStatusPhase, &phase); err != nil {
		return err
	}
	dst.Status.Phase = v1alpha2.StoragePoolPhase(phase)
	for _, field := range []struct {
		key    string
		target any
	}{
		{annoStatusClassNames, &dst.Status.StorageClassNames},
		{annoStatusDefaultClassName, &dst.Status.DefaultStorageClassName},
		{annoStatusActiveOpsRef, &dst.Status.ActiveOpsRef},
		{annoStatusMessage, &dst.Status.Message},
		{annoStatusObservedGeneraton, &dst.Status.ObservedGeneration},
	} {
		if err := unstash(meta, field.key, field.target); err != nil {
			return err
		}
	}
	return nil
}

// stash writes one value as JSON, or removes the annotation when the value is
// absent. Every field goes through JSON rather than each type getting its own
// formatting, because the set spans strings, pointers, a slice, and an integer,
// and one encoding that round-trips all of them is less to get wrong than five
// that each round-trip one.
func stash(meta *metav1.ObjectMeta, key string, value any) error {
	if isAbsent(value) {
		clear(meta, key)
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	meta.Annotations[key] = string(encoded)
	return nil
}

// unstash reads one value back and removes the annotation.
//
// An annotation that does not decode is dropped rather than failing the
// conversion. A hand-edited value is the only way to produce one, and refusing
// to convert would make the whole object unreadable — which is a worse outcome
// than losing the field that was edited, and the same reasoning the QoS
// ceilings' parsing takes.
func unstash(meta *metav1.ObjectMeta, key string, target any) error {
	raw, ok := meta.Annotations[key]
	if !ok {
		return nil
	}
	delete(meta.Annotations, key)
	if len(meta.Annotations) == 0 {
		meta.Annotations = nil
	}
	_ = json.Unmarshal([]byte(raw), target)
	return nil
}

// isAbsent reports whether a value is the one this conversion writes nothing
// for. A nil pointer, an empty string, an empty slice, and a zero generation are
// all "the hub did not state this," and an annotation for each would be nine
// keys on every pool that set none of them.
func isAbsent(value any) bool {
	switch v := value.(type) {
	case *bool:
		return v == nil
	case string:
		return v == ""
	case []string:
		return len(v) == 0
	case int64:
		return v == 0
	}
	return false
}

func clear(meta *metav1.ObjectMeta, keys ...string) {
	for _, key := range keys {
		delete(meta.Annotations, key)
	}
	if len(meta.Annotations) == 0 {
		meta.Annotations = nil
	}
}

// poolLimitsToHub gathers the pool's own ceilings, which this version spread over
// three top-level fields, into the one block the hub states them in. It returns
// nil when none of them is set, so that a pool that asked for no ceiling does not
// come back carrying an empty one.
func poolLimitsToHub(spec StoragePoolSpec) *v1alpha2.PoolLimits {
	limits := v1alpha2.PoolLimits{
		Capacity:      spec.CapacityLimit,
		MaxVolumeSize: spec.LogicalVolumeMaxSize,
	}
	if spec.QosSpec != nil {
		limits.IOPS = spec.QosSpec.IOPS
		if t := spec.QosSpec.Throughput; t != nil {
			limits.Throughput = &v1alpha2.ThroughputLimits{
				Read: t.Read, Write: t.Write, ReadWrite: t.ReadWrite,
			}
		}
	}
	if limits == (v1alpha2.PoolLimits{}) {
		return nil
	}
	return &limits
}

// poolLimitsFromHub spreads the hub's one block back over the three fields this
// version carries it in.
func poolLimitsFromHub(spec *StoragePoolSpec, limits *v1alpha2.PoolLimits) {
	if limits == nil {
		return
	}
	spec.CapacityLimit = limits.Capacity
	spec.LogicalVolumeMaxSize = limits.MaxVolumeSize
	if limits.IOPS == nil && limits.Throughput == nil {
		return
	}
	spec.QosSpec = &StoragePoolQoSSpec{IOPS: limits.IOPS}
	if t := limits.Throughput; t != nil {
		spec.QosSpec.Throughput = &StoragePoolQoSThroughputSpec{
			Read: t.Read, Write: t.Write, ReadWrite: t.ReadWrite,
		}
	}
}

// volumeDefaultsToHub gathers the per-volume defaults, which this version keeps
// in storageClassParameters plus the top-level dhchap toggle. The three the hub
// adds are not here: they have no field to come from, and restoreHubOnly reads
// them out of the annotations instead.
//
// The four QoS ceilings stop being strings here. A value this version stored
// that is not an integer never reached the control plane as one either, so it
// converts to an absent ceiling rather than making the object unreadable: a
// conversion webhook that errors takes the whole object out of reach, and the
// Enum and Minimum markers on each version are what reject a bad value.
func volumeDefaultsToHub(spec StoragePoolSpec) *v1alpha2.VolumeDefaults {
	defaults := v1alpha2.VolumeDefaults{}
	if spec.DHCHAP {
		defaults.EnableDHCHAP = ptr.To(true)
	}
	if p := spec.StorageClassParameters; p != nil {
		defaults.IOPS = parseInt32(p.QosRwIops)
		if throughput := (v1alpha2.ThroughputLimits{
			Read:      parseInt32(p.QosRMbytes),
			Write:     parseInt32(p.QosWMbytes),
			ReadWrite: parseInt32(p.QosRwMbytes),
		}); throughput != (v1alpha2.ThroughputLimits{}) {
			defaults.Throughput = &throughput
		}
		defaults.Filesystem = p.Filesystem
		defaults.EnableEncryption = p.Encryption
		defaults.Fabric = p.Fabric
		defaults.MaxNamespacesPerSubsystem = parseInt32(p.MaxNamespacePerSubsys)
		defaults.Tune2fsReservedBlocks = p.Tune2fsReservedBlocks
	}
	if defaults == (v1alpha2.VolumeDefaults{}) {
		return nil
	}
	return &defaults
}

// volumeDefaultsFromHub spreads the hub's block back over the parameters struct
// and the top-level toggle. The three fields with no home here are stashed by
// stashHubOnly rather than dropped.
func volumeDefaultsFromHub(spec *StoragePoolSpec, defaults *v1alpha2.VolumeDefaults) {
	if defaults == nil {
		return
	}
	spec.DHCHAP = defaults.EnableDHCHAP != nil && *defaults.EnableDHCHAP
	params := StorageClassParameters{
		QosRwIops:             formatInt32(defaults.IOPS),
		Filesystem:            defaults.Filesystem,
		Encryption:            defaults.EnableEncryption,
		Fabric:                defaults.Fabric,
		MaxNamespacePerSubsys: formatInt32(defaults.MaxNamespacesPerSubsystem),
		Tune2fsReservedBlocks: defaults.Tune2fsReservedBlocks,
	}
	if t := defaults.Throughput; t != nil {
		params.QosRMbytes = formatInt32(t.Read)
		params.QosWMbytes = formatInt32(t.Write)
		params.QosRwMbytes = formatInt32(t.ReadWrite)
	}
	if params == (StorageClassParameters{}) {
		return
	}
	spec.StorageClassParameters = &params
}

// poolLimitsStatusToHub renames the observed ceilings, which this version reports
// under status.qos and the hub under status.limits.
func poolLimitsStatusToHub(qos *StoragePoolQoSStatus) *v1alpha2.PoolLimitsStatus {
	if qos == nil {
		return nil
	}
	limits := &v1alpha2.PoolLimitsStatus{Host: qos.Host, IOPS: qos.IOPS}
	if t := qos.Throughput; t != nil {
		limits.Throughput = &v1alpha2.ThroughputLimits{
			Read: t.Read, Write: t.Write, ReadWrite: t.ReadWrite,
		}
	}
	return limits
}

// poolLimitsStatusFromHub is the inverse.
func poolLimitsStatusFromHub(limits *v1alpha2.PoolLimitsStatus) *StoragePoolQoSStatus {
	if limits == nil {
		return nil
	}
	qos := &StoragePoolQoSStatus{Host: limits.Host, IOPS: limits.IOPS}
	if t := limits.Throughput; t != nil {
		qos.Throughput = &StoragePoolQoSThroughputStatus{
			Read: t.Read, Write: t.Write, ReadWrite: t.ReadWrite,
		}
	}
	return qos
}

// parseInt32 reads one of this version's string-typed numbers. An empty or
// unparsable value is an absent ceiling; see volumeDefaultsToHub for why that is
// not an error.
func parseInt32(s string) *int32 {
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return nil
	}
	v := int32(n)
	return &v
}

// formatInt32 writes one back. An absent ceiling becomes the empty string rather
// than `0`, because `0` means unlimited in this vocabulary and unset does not.
func formatInt32(v *int32) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(int64(*v), 10)
}

// stashRemoved records a field this version has and the hub does not. An empty
// value writes no annotation, so an object that set neither removed field is not
// given metadata it never had.
func stashRemoved(meta *metav1.ObjectMeta, key, value string) {
	if value == "" {
		return
	}
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	meta.Annotations[key] = value
}

// unstashRemoved takes one back out and removes the annotation, so that an
// object converted down carries the field rather than both the field and the
// note about it.
func unstashRemoved(meta *metav1.ObjectMeta, key string) string {
	value := meta.Annotations[key]
	if value != "" {
		clear(meta, key)
	}
	return value
}
