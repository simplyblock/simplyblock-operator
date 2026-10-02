// The failure-domain mapping an expansion writes onto the cluster: how labels
// are given indices, and that both a created and a joined cluster receive one.

package deployment

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

type domains = []simplyblockv1alpha2.FailureDomainIndex

func TestFailureDomainLabelsAreIndexedInNameOrder(t *testing.T) {
	got := assignFailureDomains(nil, []string{"rack-c", "rack-a", "rack-b", "rack-a"})
	want := domains{{Name: "rack-a", Index: 0}, {Name: "rack-b", Index: 1}, {Name: "rack-c", Index: 2}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mapping (-want +got):\n%s", diff)
	}
}

// An index the control plane already holds nodes under cannot move, so a
// growth keeps every assigned label and only appends the new ones.
func TestAssignedFailureDomainsKeepTheirIndex(t *testing.T) {
	assigned := domains{{Name: "rack-b", Index: 0}, {Name: "rack-d", Index: 2}}
	got := assignFailureDomains(assigned, []string{"rack-a", "rack-b", "rack-c"})
	want := domains{
		{Name: "rack-b", Index: 0},
		{Name: "rack-a", Index: 1},
		{Name: "rack-d", Index: 2},
		{Name: "rack-c", Index: 3},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mapping (-want +got):\n%s", diff)
	}
}

// A numeric label was sent as its own number before the mapping existed, so it
// keeps that number whenever the number is free.
func TestANumericLabelClaimsItsOwnIndex(t *testing.T) {
	got := assignFailureDomains(nil, []string{"rack-a", "2", "0"})
	want := domains{{Name: "0", Index: 0}, {Name: "rack-a", Index: 1}, {Name: "2", Index: 2}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mapping (-want +got):\n%s", diff)
	}
}

func TestANumericLabelWhoseIndexIsTakenGetsTheNextFreeOne(t *testing.T) {
	got := assignFailureDomains(domains{{Name: "rack-a", Index: 0}}, []string{"0"})
	want := domains{{Name: "rack-a", Index: 0}, {Name: "0", Index: 1}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mapping (-want +got):\n%s", diff)
	}
}

func TestADocumentWithoutFailureDomainsMapsNothing(t *testing.T) {
	if got := assignFailureDomains(nil, nil); got != nil {
		t.Errorf("mapping = %v, want none", got)
	}
}

func documentWithDomains(
	mutate func(*simplyblockv1alpha2.ClusterDeploymentConfig),
) *simplyblockv1alpha2.ClusterDeploymentConfig {
	return aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		group := c.Spec.NodeSets[0].Groups[0]
		group.Workers = []string{"worker-1"}
		group.FailureDomain = "rack-b"
		other := group
		other.Name = "jupiter"
		other.Workers = []string{"worker-2"}
		other.FailureDomain = "rack-a"
		c.Spec.NodeSets[0].Groups = []simplyblockv1alpha2.NodeGroup{group, other}
		if mutate != nil {
			mutate(c)
		}
	})
}

func clusterDomains(t *testing.T, r *ClusterDeploymentConfigReconciler) domains {
	t.Helper()
	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: theNamespace, Name: theCluster}
	if err := r.Get(context.Background(), key, &cluster); err != nil {
		t.Fatalf("reading the cluster: %v", err)
	}
	return cluster.Status.FailureDomains
}

func TestCreatingNodesMapsTheDocumentsFailureDomains(t *testing.T) {
	config := documentWithDomains(nil)
	config.Status.ClusterRef = theCluster
	objects := append(workers("worker-1", "worker-2"), config, aCluster(nil))
	r := reconcilerFor(t, objects...)

	if _, err := r.createNodes(context.Background(), config); err != nil {
		t.Fatalf("createNodes: %v", err)
	}

	want := domains{{Name: "rack-a", Index: 0}, {Name: "rack-b", Index: 1}}
	if diff := cmp.Diff(want, clusterDomains(t, r)); diff != "" {
		t.Errorf("status.failureDomains (-want +got):\n%s", diff)
	}
}

// A growth document joins a cluster that already has a mapping, and adds to it
// without moving what is there.
func TestAGrowthDocumentExtendsTheClustersMapping(t *testing.T) {
	config := documentWithDomains(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.ClusterRef = theCluster
		c.Spec.Cluster = nil
	})
	config.Status.ClusterRef = theCluster
	cluster := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.FailureDomains = domains{{Name: "rack-b", Index: 0}}
	})
	objects := append(workers("worker-1", "worker-2"), config, cluster)
	r := reconcilerFor(t, objects...)

	if _, err := r.createNodes(context.Background(), config); err != nil {
		t.Fatalf("createNodes: %v", err)
	}

	want := domains{{Name: "rack-b", Index: 0}, {Name: "rack-a", Index: 1}}
	if diff := cmp.Diff(want, clusterDomains(t, r)); diff != "" {
		t.Errorf("status.failureDomains (-want +got):\n%s", diff)
	}
}
