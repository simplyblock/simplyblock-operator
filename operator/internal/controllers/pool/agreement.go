// Whether a class assigned to a pool agrees with the pool (design-storagepool.md
// §5): what the operator reports about an authored class, and what the
// StorageClass admission webhook refuses.
//
// A class is authored, and a pool may back several that differ on purpose, one
// ext4 and one XFS, or one with a tighter QoS ceiling. So most of
// spec.volumeDefaults is a default a class may override, and an override is
// information. Two things are not: the cluster and pool the class reaches,
// which is where its volumes are provisioned, and the stripe, which the pool's
// admission checks (its node count, a stretched cluster) were made for. A
// class departing from either is a disagreement.

package pool

import (
	"fmt"
	"sort"
	"strings"

	"github.com/simplyblock/atlas/kube"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// ClassAgreement is how a class's parameters stand against its pool.
type ClassAgreement struct {
	// Disagreements are departures a class may not make: another cluster or
	// pool, or another stripe. One line each.
	Disagreements []string
	// Overrides are volume defaults the class sets otherwise than the pool,
	// which it may. One line each.
	Overrides []string
}

// CompareClass reads a class's parameters against pool p. clusterUUID is the
// cluster the pool's volumes are provisioned in, the supercluster's for a
// supercluster pool; empty skips the cluster check, for a cluster that has no
// UUID yet.
func CompareClass(params map[string]string, p *simplyblockv1alpha2.StoragePool, clusterUUID string) ClassAgreement {
	var a ClassAgreement
	want := ClassParameters(p, clusterUUID)

	if clusterUUID != "" && strings.TrimSpace(params[kube.ParamClusterID]) != clusterUUID {
		a.Disagreements = append(a.Disagreements, fmt.Sprintf("%s is %q, and the pool is in cluster %s",
			kube.ParamClusterID, params[kube.ParamClusterID], clusterUUID))
	}
	if pool := strings.TrimSpace(params[kube.ParamPool]); pool != p.Name && (p.Status.UUID == "" || pool != p.Status.UUID) {
		a.Disagreements = append(a.Disagreements, fmt.Sprintf("%s is %q, and the pool is %s",
			kube.ParamPool, params[kube.ParamPool], p.Name))
	}
	if d := compareStripe(params, want); d != "" {
		a.Disagreements = append(a.Disagreements, d)
	}

	keys := make([]string, 0, len(want))
	for key := range want {
		switch key {
		case kube.ParamClusterID, kube.ParamPool, kube.ParamStripeWidth, kube.ParamStripeSize:
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if got := classValue(params, key); got != want[key] {
			a.Overrides = append(a.Overrides, fmt.Sprintf("%s is %q, and the pool's default is %q", key, got, want[key]))
		}
	}
	return a
}

// compareStripe compares the stripe the class asks for with the pool's, read
// the way the driver reads both (kube.ParseStripe), so that one size in two
// spellings is one size. It returns the disagreement, or "" for none.
func compareStripe(params, want map[string]string) string {
	pool, err := kube.ParseStripe(want)
	if err != nil {
		// The pool's own CEL rules refuse what ParseStripe would; a pool that
		// got past them is not the class's disagreement.
		return ""
	}
	class, err := kube.ParseStripe(params)
	switch {
	case err != nil:
		return err.Error()
	case class.Width != pool.Width:
		return fmt.Sprintf("%s is %d, and the pool's volumes are striped across %d",
			kube.ParamStripeWidth, class.Width, pool.Width)
	case class.ChunkBytes != pool.ChunkBytes:
		return fmt.Sprintf("%s is %d bytes, and the pool's is %d",
			kube.ParamStripeSize, class.ChunkBytes, pool.ChunkBytes)
	}
	return ""
}

// classValue is what the class says for a key the pool writes, reading a QoS
// ceiling under any of its spellings (design-storagepool.md §5.1), since a class
// may carry an older one indefinitely.
func classValue(params map[string]string, key string) string {
	for _, ceiling := range kube.QoSCeilings() {
		if kube.QoSParamKeys(ceiling)[0] == key {
			return strings.TrimSpace(kube.QoSParam(params, ceiling))
		}
	}
	return strings.TrimSpace(params[key])
}
