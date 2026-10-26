package deployment

// A role swap on an expanded stretched cluster: the hub re-delivers the site's
// approved document with the new role, and the site's StorageCluster takes it
// up (design-stretched-sync-replication-ramen.md §11.1).

import (
	"context"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func expandedStretched(role simplyblockv1alpha2.StretchedRole) *simplyblockv1alpha2.ClusterDeploymentConfig {
	return aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.Stretched = &simplyblockv1alpha2.StretchedSpec{Site: "a", PeerSite: "b", Role: role}
		c.Status.Phase = simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanded
		c.Status.ClusterRef = theCluster
	})
}

func stretchedClusterWithRole(role simplyblockv1alpha2.StretchedRole) *simplyblockv1alpha2.StorageCluster {
	return aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.Stretched = &simplyblockv1alpha2.StretchedSpec{Site: "a", PeerSite: "b", Role: role}
	})
}

func roleOf(t *testing.T, r *ClusterDeploymentConfigReconciler) simplyblockv1alpha2.StretchedRole {
	t.Helper()
	var c simplyblockv1alpha2.StorageCluster
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: theNamespace, Name: theCluster}, &c); err != nil {
		t.Fatal(err)
	}
	return c.Spec.Stretched.Role
}

// Regression: 2026-10-26-role-swap-never-delivered: the expansion runs once and
// an expanded document was terminal, so a role the hub re-delivered never
// reached the site's StorageCluster. The role of an expanded document is
// carried onto its cluster.
func TestAnExpandedDocumentsRoleReachesItsCluster(t *testing.T) {
	config := expandedStretched(simplyblockv1alpha2.StretchedRoleFollower)
	r := reconcilerFor(t, config, stretchedClusterWithRole(simplyblockv1alpha2.StretchedRoleLeader))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(config)}); err != nil {
		t.Fatal(err)
	}
	if got := roleOf(t, r); got != simplyblockv1alpha2.StretchedRoleFollower {
		t.Errorf("the cluster's role is %q, want the document's Follower", got)
	}
}

// Only the role is carried: an expanded document is otherwise still the record
// of what was deployed, and a cluster that is not stretched is left alone.
func TestOnlyTheRoleOfAnExpandedDocumentIsCarried(t *testing.T) {
	config := expandedStretched(simplyblockv1alpha2.StretchedRoleFollower)
	config.Spec.Cluster.VCPUCount = nil
	cluster := stretchedClusterWithRole(simplyblockv1alpha2.StretchedRoleLeader)
	r := reconcilerFor(t, config, cluster)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(config)}); err != nil {
		t.Fatal(err)
	}
	var got simplyblockv1alpha2.StorageCluster
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(cluster), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.VCPUCount == nil || *got.Spec.VCPUCount != *cluster.Spec.VCPUCount {
		t.Errorf("vcpuCount = %v; the expansion's other fields must not be carried", got.Spec.VCPUCount)
	}
}
