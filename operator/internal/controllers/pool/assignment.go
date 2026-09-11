// How a StorageClass says which StoragePool it draws from, and how a pool finds
// the classes assigned to it.
//
// A class and a pool are two halves of one contract — the pool is the capacity
// and its ceilings, the class is how a claim asks for some — and nothing in the
// Kubernetes API joins them. A StorageClass is cluster-scoped and a StoragePool
// is namespaced, so the class cannot be an owned child: garbage collection
// deletes a cluster-scoped object whose owner is namespaced. What joins them is
// three labels, and this file is the only place that knows it.
//
// The link is in labels rather than in the class's name, and that is what makes
// zero-or-more classes per pool possible. A class named for its pool puts the
// link in the one field that has to be unique, so a pool could have exactly one
// class, every controller needing the link would recompute the string, and
// asking a class which pool it draws from would mean parsing its name. A label
// carries it instead: two classes cannot share a name and can share a label, the
// answer is a selector rather than a computation, and a class may be called
// whatever its author finds useful.
//
// design-storagepool.md §5 is the specification.

package pool

import (
	"context"
	"fmt"
	"sort"

	storagev1 "k8s.io/api/storage/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The three labels that are the assignment. All three are needed and none is
// redundant: a pool name is unique within a namespace and a cluster, so a class
// naming only the pool would match a pool of the same name in another namespace
// or another cluster of the same namespace.
const (
	// LabelNamespace is the namespace the assigned StoragePool lives in.
	LabelNamespace = "storage.simplyblock.io/namespace"
	// LabelCluster is the StorageCluster the pool is carved out of.
	LabelCluster = "storage.simplyblock.io/cluster"
	// LabelPool is the StoragePool the class draws from.
	LabelPool = "storage.simplyblock.io/pool"

	// LabelManagedBy marks a class this operator wrote, and it is the whole of
	// the difference between the two kinds of class. A class carrying it was
	// created by the operator, so the operator may delete it when its pool goes;
	// a class without it was written by somebody else, so the operator may not.
	// One label, one rule, and it is the same principle either way round.
	LabelManagedBy = "storage.simplyblock.io/managed-by"

	// ManagedByStorageCluster is the value LabelManagedBy carries on the class
	// written alongside a cluster's default pool. It names what created the
	// class rather than what owns it, because nothing owns it: the scopes forbid
	// an owner reference.
	ManagedByStorageCluster = "storagecluster"
)

// AssignmentLabels are the three labels a class assigned to this pool carries.
// They are both what the operator writes on a class it creates and what it
// selects on to find the classes somebody else wrote.
func AssignmentLabels(p *simplyblockv1alpha2.StoragePool) map[string]string {
	return map[string]string{
		LabelNamespace: p.Namespace,
		LabelCluster:   p.Spec.ClusterRef,
		LabelPool:      p.Name,
	}
}

// DefaultStorageClassName is the name of the class the operator writes for a
// cluster's default pool.
//
// The namespace is in the name because a StorageClass is cluster-scoped and a
// StorageCluster is not: two namespaces may each hold a cluster called
// production, and a name derived from the cluster alone would have the second
// default pool collide with the first one's class instead of getting its own.
//
// It is a derived name only because the operator has to choose one, and nothing
// reads it back: the pool records what was written in
// status.defaultStorageClassName, and every other question about which classes a
// pool has is answered by AssignmentLabels. An authored class is called whatever
// its author wants.
func DefaultStorageClassName(namespace, clusterName string) string {
	return "simplyblock-" + namespace + "-" + clusterName
}

// DefaultPoolName is the name of the pool created alongside a cluster, so that a
// cluster can hold volumes without anybody authoring a pool first.
func DefaultPoolName(clusterName string) string {
	return clusterName + "-default"
}

// AssignedClasses returns the StorageClasses assigned to a pool, by name and
// sorted, together with the objects themselves.
//
// Sorting is not cosmetic. The names are published as status.storageClassNames,
// and a list whose order came from the API server's would rewrite the status
// every time a list came back differently, which is a write per reconcile for a
// set that did not change.
func AssignedClasses(
	ctx context.Context, reader client.Reader, p *simplyblockv1alpha2.StoragePool,
) ([]storagev1.StorageClass, []string, error) {
	var classes storagev1.StorageClassList
	if err := reader.List(ctx, &classes, client.MatchingLabels(AssignmentLabels(p))); err != nil {
		return nil, nil, fmt.Errorf("list the storage classes assigned to pool %s/%s: %w",
			p.Namespace, p.Name, err)
	}

	names := make([]string, 0, len(classes.Items))
	for i := range classes.Items {
		names = append(names, classes.Items[i].Name)
	}
	sort.Strings(names)
	sort.Slice(classes.Items, func(i, j int) bool {
		return classes.Items[i].Name < classes.Items[j].Name
	})
	return classes.Items, names, nil
}

// IsOperatorManaged reports whether the operator wrote this class and may
// therefore delete it.
//
// The value is compared rather than merely tested for presence. The label is the
// whole of the difference between a class the operator cleans up and one it
// refuses to touch, so an authored class carrying somebody else's managed-by
// must not be read as this operator's and deleted along with the pool.
func IsOperatorManaged(class *storagev1.StorageClass) bool {
	return class != nil && class.Labels[LabelManagedBy] == ManagedByStorageCluster
}

// ClassParameters builds the parameters a class assigned to this pool carries.
//
// cluster_id and pool_name are what the driver needs to reach the backend; the
// rest is the pool's spec.volumeDefaults, written under the current QoS spelling
// only. A class the operator generates carries one generation, so nothing it
// creates needs the fallback the driver keeps for the older keys.
//
// Nothing is invented. Whatever the pool's defaults are is what the class
// states, because StorageClass.parameters is immutable and a guessed filesystem,
// fabric, or ceiling would be one nobody could edit afterward.
func ClassParameters(p *simplyblockv1alpha2.StoragePool, clusterUUID string) map[string]string {
	params := map[string]string{
		kube.ParamClusterID: clusterUUID,
		kube.ParamPool:      p.Name,
	}

	defaults := p.Spec.VolumeDefaults
	if defaults == nil {
		return params
	}

	setInt := func(key string, value *int32) {
		if value != nil {
			params[key] = fmt.Sprintf("%d", *value)
		}
	}
	setBool := func(key string, value *bool) {
		if value != nil {
			params[key] = fmt.Sprintf("%t", *value)
		}
	}

	setInt(kube.ParamMaxIOPS, defaults.IOPS)
	if t := defaults.Throughput; t != nil {
		setInt(kube.ParamMaxMBytesPerSec, t.ReadWrite)
		setInt(kube.ParamMaxReadMBytesPerSec, t.Read)
		setInt(kube.ParamMaxWriteMBytesPerSec, t.Write)
	}
	setBool(kube.ParamEncryption, defaults.EnableEncryption)
	setBool(kube.ParamCompression, defaults.EnableCompression)
	setBool(kube.ParamReplication, defaults.EnableReplication)
	setInt(kube.ParamMaxNamespacePerSubsys, defaults.MaxNamespacesPerSubsystem)
	if defaults.PriorityClass != "" {
		params[kube.ParamPriorityClass] = defaults.PriorityClass
	}
	if defaults.Filesystem != "" {
		params[paramFSType] = defaults.Filesystem
	}
	if defaults.Fabric != "" {
		params[kube.ParamFabric] = defaults.Fabric
	}
	if defaults.Tune2fsReservedBlocks != "" {
		params[paramTune2fsReservedBlocks] = defaults.Tune2fsReservedBlocks
	}

	// DHCHAP restricts the pool's volumes to the nodes it allows, through one
	// label per pool on each of them, and the class republishes the key so the
	// driver can turn it into the volume's node affinity. The pool's UUID is in
	// the key, so this is only reachable once the pool exists in the control
	// plane, which is why the default class is written after creation rather
	// than beside it.
	if defaults.EnableDHCHAP != nil && *defaults.EnableDHCHAP &&
		len(p.Spec.AllowedNodes) > 0 && p.Status.UUID != "" {
		params[paramDHCHAPNodeSelector] = kube.PoolNodeLabelKey(p.Status.UUID)
	}

	return params
}

// The class parameters that are not in atlas-lib's set, because they belong to
// something other than the control plane's own vocabulary.
const (
	// paramFSType is Kubernetes' own well-known key for the filesystem an
	// external provisioner formats with, which is why it is not spelled the way
	// this group's keys are.
	paramFSType = "csi.storage.k8s.io/fstype"
	// paramTune2fsReservedBlocks is the ext4 reserved-block percentage the node
	// plugin passes to tune2fs. An absent key and "0" are different: the plugin
	// skips the call entirely only when the value is empty.
	paramTune2fsReservedBlocks = "tune2fs_reserved_blocks"
	// paramDHCHAPNodeSelector is the exact allowed-node label key, read by
	// paramDHCHAPNodeSelector in csi-driver/internal/csi/controller/params.go.
	paramDHCHAPNodeSelector = "dhchap_node_selector"
)

// ConsumingClassName returns the name of a StorageClass that provisions out of
// the named pool, for a caller that has to write a PersistentVolume or a claim
// and needs a class to name.
//
// It prefers the class the operator wrote for a default pool and otherwise takes
// the first assigned class in name order, which is arbitrary but stable. A pool
// with no class at all is a valid state, so the absence is reported rather than
// invented: the caller has no class to name and has to say so.
func ConsumingClassName(
	ctx context.Context, reader client.Reader, namespace, clusterName, poolName string,
) (string, error) {
	var pools simplyblockv1alpha2.StoragePoolList
	if err := reader.List(ctx, &pools, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list the storage pools in %s: %w", namespace, err)
	}
	for i := range pools.Items {
		p := &pools.Items[i]
		if p.Name != poolName || p.Spec.ClusterRef != clusterName {
			continue
		}
		if p.Status.DefaultStorageClassName != "" {
			return p.Status.DefaultStorageClassName, nil
		}
		if len(p.Status.StorageClassNames) > 0 {
			return p.Status.StorageClassNames[0], nil
		}
		return "", fmt.Errorf("storage pool %s/%s has no storage class assigned to it",
			namespace, poolName)
	}
	return "", fmt.Errorf("no storage pool %q of cluster %q in namespace %s",
		poolName, clusterName, namespace)
}
