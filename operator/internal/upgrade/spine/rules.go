// What becomes of each kind a retiring StorageNodeSet owns.
//
// The list is declarative for the same reason §12.2's classification is: an
// object in the set that nobody classified stops the upgrade. Neither
// disposition is safe as a default. Reparenting everything adopts objects the
// target model has no owner for, and leaving everything behind deletes a
// running storage system when the set goes, so a kind somebody adds to the
// StorageNodeSet controller without deciding its disposition fails the
// preflight rather than silently taking the wrong branch.

package spine

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Disposition is what the migration does with a dependent.
type Disposition string

const (
	// Reparent moves the object's controller reference to the StorageCluster.
	// It is what every kind in the list does today, because §16.1 makes the
	// cluster the parent of everything the set held.
	Reparent Disposition = "reparent onto the StorageCluster"

	// Delete removes the object with its owner. It is declared for a kind the
	// target model has no use for, and there is none today.
	Delete Disposition = "delete with the StorageNodeSet"
)

// Rule says what becomes of one kind a StorageNodeSet owns.
type Rule struct {
	// Kind is the dependent's kind.
	Kind schema.GroupVersionKind

	// Does is what the migration does with it.
	Does Disposition

	// Why says what the object is for, so a plan naming it reads as something
	// other than a list of Kubernetes nouns.
	Why string
}

// Rules is the disposition of every kind a StorageNodeSet owns by controller
// reference today, read from simplyblockstoragenodeset_controller.go and its
// neighbors rather than from the design.
//
// The ClusterRole and ClusterRoleBinding the same controller creates are
// deliberately absent. They are cluster-scoped, so Kubernetes does not let a
// namespaced object own them and no owner reference is set, which means they
// already survive the set's deletion and the retirement does nothing to them.
func Rules() []Rule {
	return []Rule{
		{
			Kind: gvk("storage.simplyblock.io", "v1alpha1", "StorageNode"),
			Does: Reparent,
			Why:  "the storage node itself, and the one real ownership edge in the spine",
		},
		{
			Kind: gvk("apps", "v1", "DaemonSet"),
			Does: Reparent,
			Why:  "the workload that runs the storage nodes, whose recreation restarts every node plugin at once",
		},
		{
			Kind: gvk("", "v1", "Service"),
			Does: Reparent,
			Why:  "the storage-node API and SPDK proxy Services the control plane reaches a node through",
		},
		{
			Kind: gvk("discovery.k8s.io", "v1", "EndpointSlice"),
			Does: Reparent,
			Why:  "what publishes a set's API pods behind the shared headless Service",
		},
		{
			Kind: gvk("", "v1", "ServiceAccount"),
			Does: Reparent,
			Why:  "the identity the storage-node DaemonSet runs as",
		},
		{
			Kind: gvk("", "v1", "ConfigMap"),
			Does: Reparent,
			Why:  "the per-node configuration, one key per worker hostname",
		},
		{
			Kind: gvk("", "v1", "Secret"),
			Does: Reparent,
			Why:  "the serving certificate's key material",
		},
		{
			Kind: gvk("cert-manager.io", "v1", "Certificate"),
			Does: Reparent,
			Why:  "the serving certificate, where cert-manager is the TLS provider",
		},
	}
}

// ruleFor returns the rule covering a kind, and whether one exists.
func ruleFor(rules []Rule, kind schema.GroupVersionKind) (Rule, bool) {
	for _, rule := range rules {
		if rule.Kind == kind {
			return rule, true
		}
	}
	return Rule{}, false
}

func gvk(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}
