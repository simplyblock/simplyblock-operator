package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"

	"github.com/simplyblock/atlas/errs"
)

// QoSLimits are the per-volume quality-of-service caps a StorageClass may set.
// A zero field means "unset" (no limit).
type QoSLimits struct {
	// RWIOPS caps combined read+write IOPS (qos_rw_iops).
	RWIOPS int
	// RWMBytes caps combined read+write throughput in MiB/s (qos_rw_mbytes).
	RWMBytes int
	// RMBytes caps read throughput in MiB/s (qos_r_mbytes).
	RMBytes int
	// WMBytes caps write throughput in MiB/s (qos_w_mbytes).
	WMBytes int
}

// Properties are the provisioning parameters parsed from a StorageClass — the
// full set the CSI controller reads at CreateVolume, in typed form. It is the
// control-plane-side view of how a volume was provisioned, available to any
// component with StorageClass access (e.g. the operator's rebalancer), as
// opposed to the host sysfs view exposed by the nvme package.
type Properties struct {
	// Pool is the storage pool the volume is created in (pool_name).
	Pool string
	// Fabric is the transport fabric (fabric), e.g. "tcp".
	Fabric string
	// ClusterID pins provisioning to a specific storage cluster (cluster_id);
	// empty when the class does not target one explicitly.
	ClusterID string
	// MaxSize caps volume growth (max_size); empty when unset. Kept as the raw
	// string because the control plane accepts size suffixes (e.g. "10G").
	MaxSize string
	// LvolPriorityClass is the logical-volume priority class (lvol_priority_class).
	LvolPriorityClass int
	// MaxNamespacePerSubsys is how many NVMe namespaces may share one subsystem
	// (max_namespace_per_subsys). A value > 1 makes volumes of this class
	// "namespaced" — they share a subsystem with siblings; see IsMultiNamespace.
	MaxNamespacePerSubsys int
	// Compression enables volume compression (compression).
	Compression bool
	// Encryption enables volume encryption (encryption).
	Encryption bool
	// Replicate enables cross-cluster replication (replicate).
	Replicate bool
	// QoS holds the quality-of-service caps.
	QoS QoSLimits
}

// IsMultiNamespace reports whether volumes provisioned by this class share an
// NVMe subsystem with sibling volumes (max_namespace_per_subsys > 1). Such a
// volume cannot be migrated or rebalanced on its own — moving it would disturb
// every other volume sharing the subsystem.
//
// This is the StorageClass-based counterpart to nvme.Subsystem.IsMultiNamespace:
// same question, answered from the provisioning parameters the operator can read
// centrally, rather than from host sysfs it cannot.
func (p Properties) IsMultiNamespace() bool {
	return p.MaxNamespacePerSubsys > 1
}

// PropertiesFromStorageClass parses the atlas-recognized provisioning parameters
// from sc. It returns errs.ErrUnsupported if sc is nil or not provisioned by this
// driver. A present-but-malformed numeric parameter yields an error (a
// misconfigured StorageClass should surface, not be silently defaulted).
func PropertiesFromStorageClass(sc *storagev1.StorageClass) (Properties, error) {
	if sc == nil || sc.Provisioner != DriverName {
		return Properties{}, fmt.Errorf("storageclass: %w", errs.ErrUnsupported)
	}
	p := sc.Parameters

	priorClass, err := IntParam(p, ParamLvolPriorityClass, 0)
	if err != nil {
		return Properties{}, err
	}
	maxNS, err := IntParam(p, ParamMaxNamespacePerSubsys, 1)
	if err != nil {
		return Properties{}, err
	}
	compression, err := BoolParam(p, ParamCompression, false)
	if err != nil {
		return Properties{}, err
	}
	encryption, err := BoolParam(p, ParamEncryption, false)
	if err != nil {
		return Properties{}, err
	}
	replicate, err := BoolParam(p, ParamReplicate, false)
	if err != nil {
		return Properties{}, err
	}
	qos, err := qosFromParams(p)
	if err != nil {
		return Properties{}, err
	}

	return Properties{
		Pool:                  StringParam(p, ParamPool, ""),
		Fabric:                StringParam(p, ParamFabric, ""),
		ClusterID:             StringParam(p, ParamClusterID, ""),
		MaxSize:               StringParam(p, ParamMaxSize, ""),
		LvolPriorityClass:     priorClass,
		MaxNamespacePerSubsys: maxNS,
		Compression:           compression,
		Encryption:            encryption,
		Replicate:             replicate,
		QoS:                   qos,
	}, nil
}

func qosFromParams(p map[string]string) (QoSLimits, error) {
	rwIOPS, err := IntParam(p, ParamQoSRWIOPS, 0)
	if err != nil {
		return QoSLimits{}, err
	}
	rwMB, err := IntParam(p, ParamQoSRWMBytes, 0)
	if err != nil {
		return QoSLimits{}, err
	}
	rMB, err := IntParam(p, ParamQoSRMBytes, 0)
	if err != nil {
		return QoSLimits{}, err
	}
	wMB, err := IntParam(p, ParamQoSWMBytes, 0)
	if err != nil {
		return QoSLimits{}, err
	}
	return QoSLimits{RWIOPS: rwIOPS, RWMBytes: rwMB, RMBytes: rMB, WMBytes: wMB}, nil
}

// StorageClassNameFromPV returns the name of the StorageClass that provisioned
// pv, and whether one is set. It reads Spec.StorageClassName, falling back to
// the legacy beta annotation for PVs created before it was promoted.
func StorageClassNameFromPV(pv *corev1.PersistentVolume) (string, bool) {
	if pv == nil {
		return "", false
	}
	if name := pv.Spec.StorageClassName; name != "" {
		return name, true
	}
	if name := pv.Annotations["volume.beta.kubernetes.io/storage-class"]; name != "" {
		return name, true
	}
	return "", false
}

// ResolvePropertiesForPV resolves the StorageClass that provisioned pv via r and
// returns its parsed Properties. It returns errs.ErrUnsupported if pv is not a
// volume managed by this driver, and errs.ErrNotFound if pv names no
// StorageClass. Only Resolver.StorageClassByName is used, so a consumer with a
// narrower need can pass any type implementing that method.
func ResolvePropertiesForPV(ctx context.Context, r Resolver, pv *corev1.PersistentVolume) (Properties, error) {
	if !IsManaged(pv) {
		return Properties{}, fmt.Errorf("pv %q: %w", pvName(pv), errs.ErrUnsupported)
	}
	name, ok := StorageClassNameFromPV(pv)
	if !ok {
		return Properties{}, fmt.Errorf("pv %q: no storage class: %w", pvName(pv), errs.ErrNotFound)
	}
	sc, err := r.StorageClassByName(ctx, name)
	if err != nil {
		return Properties{}, fmt.Errorf("pv %q: storage class %q: %w", pvName(pv), name, err)
	}
	return PropertiesFromStorageClass(sc)
}
