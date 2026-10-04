// Reading a pool's two groups of ceiling without a nil check at every call site.
//
// spec.limits, spec.volumeDefaults, and each throughput block below them are all
// optional pointers, so every value a caller wants is three dereferences deep and
// any of them may be absent. Written out at the call sites that is one guard per
// field per caller, and the guards are where a mistake hides: a missed one is a
// panic, and a wrong one silently sends a volume's default where the pool's
// ceiling belonged. These accessors are the one place that knows the shape.
//
// The two groups deliberately have separate accessors rather than one
// parameterized pair. They go to different places — the pool's ceilings to the
// control plane, the volume's defaults to a StorageClass — and a helper that
// could return either is a helper that can return the wrong one, which is the
// exact confusion the regrouping exists to end.

package pool

import (
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// direction names which of the three throughput ceilings is wanted.
type direction int

const (
	read direction = iota
	write
	readWrite
)

func limitCapacity(p *simplyblockv1alpha2.StoragePool) string {
	if p.Spec.Limits == nil {
		return ""
	}
	return p.Spec.Limits.Capacity
}

func limitMaxVolumeSize(p *simplyblockv1alpha2.StoragePool) string {
	if p.Spec.Limits == nil {
		return ""
	}
	return p.Spec.Limits.MaxVolumeSize
}

func limitIOPS(p *simplyblockv1alpha2.StoragePool) *int32 {
	if p.Spec.Limits == nil {
		return nil
	}
	return p.Spec.Limits.IOPS
}

func limitThroughput(p *simplyblockv1alpha2.StoragePool, d direction) *int32 {
	if p.Spec.Limits == nil {
		return nil
	}
	return throughput(p.Spec.Limits.Throughput, d)
}

func throughput(t *simplyblockv1alpha2.ThroughputLimits, d direction) *int32 {
	if t == nil {
		return nil
	}
	switch d {
	case read:
		return t.Read
	case write:
		return t.Write
	case readWrite:
		return t.ReadWrite
	}
	return nil
}

// volumeDefaultsDHCHAP reports whether the pool's volumes authenticate. It is
// the one volume default the control plane is told about directly rather than
// through a StorageClass, because the pool's own key material depends on it.
func volumeDefaultsDHCHAP(p *simplyblockv1alpha2.StoragePool) bool {
	d := p.Spec.VolumeDefaults
	return d != nil && d.EnableDHCHAP != nil && *d.EnableDHCHAP
}

// int32Value is the zero-for-absent reading the control plane's vocabulary
// wants: a ceiling it is not told about is the same as no ceiling.
func int32Value(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}

// parseSize reads a capacity written the way an administrator writes one: `10T`,
// `500G`. An unparsable or empty value is zero, which is the control plane's
// spelling of unlimited.
func parseSize(s string) int64 {
	if size := utils.ParseSize(s, "si/iec", "", false); size != nil {
		return *size
	}
	return 0
}

// limitsStatusFromDTO is what the control plane says the ceilings actually are,
// which is not necessarily what spec.limits asked for.
func limitsStatusFromDTO(dto poolDTO) *simplyblockv1alpha2.PoolLimitsStatus {
	iops := int32(dto.MaxRwIOPS)
	rw, rd, wr := int32(dto.MaxRwMbytes), int32(dto.MaxRMbytes), int32(dto.MaxWMbytes)
	return &simplyblockv1alpha2.PoolLimitsStatus{
		Host: dto.QoSHost,
		IOPS: &iops,
		Throughput: &simplyblockv1alpha2.ThroughputLimits{
			Read: &rd, Write: &wr, ReadWrite: &rw,
		},
	}
}

func nodeNames(nodes []corev1.Node) []string {
	if len(nodes) == 0 {
		return nil
	}
	names := make([]string, 0, len(nodes))
	for i := range nodes {
		names = append(names, nodes[i].Name)
	}
	return names
}

// equality compares two statuses semantically, so that a nil slice and an empty
// one are the same thing. It is what lets a steady-state reconcile patch nothing
// rather than writing an identical status to etcd on every resync.
func equality(a, b simplyblockv1alpha2.StoragePoolStatus) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}
