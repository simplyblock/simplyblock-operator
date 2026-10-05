// The failure-domain mapping an expansion writes onto the cluster: how labels
// are given indices, and that both a created and a joined cluster receive one.

package deployment

import (
	"context"
	"fmt"
	"strings"
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

// documentWithDomainCount is a document whose groups declare count distinct
// failure-domain labels, rack-0 onward.
func documentWithDomainCount(
	count int, mutate func(*simplyblockv1alpha2.ClusterDeploymentConfig),
) *simplyblockv1alpha2.ClusterDeploymentConfig {
	return aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		template := c.Spec.NodeSets[0].Groups[0]
		groups := make([]simplyblockv1alpha2.NodeGroup, 0, count)
		for i := range count {
			group := template
			group.Name = fmt.Sprintf("group-%d", i)
			group.Workers = []string{fmt.Sprintf("worker-%d", i)}
			group.FailureDomain = fmt.Sprintf("rack-%d", i)
			groups = append(groups, group)
		}
		c.Spec.NodeSets[0].Groups = groups
		if mutate != nil {
			mutate(c)
		}
	})
}

func tooManyDomains(t *testing.T, r *ClusterDeploymentConfigReconciler,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) []finding {
	t.Helper()
	findings, err := r.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	var found []finding
	for _, f := range findings {
		if f.reason == TooManyFailureDomains {
			found = append(found, f)
		}
	}
	return found
}

// The cluster's mapping holds at most maxFailureDomains entries, and a status
// patch past that is refused by the apiserver, which would leave CreatingNodes
// retrying forever. The draft says so instead, before anybody approves it.
func TestADocumentWithMoreFailureDomainsThanTheMappingHoldsIsRefused(t *testing.T) {
	config := documentWithDomainCount(maxFailureDomains+1, nil)
	r := reconcilerFor(t, config)

	found := tooManyDomains(t, r, config)
	if len(found) != 1 {
		t.Fatalf("findings = %+v, want one TooManyFailureDomains", found)
	}
	if !strings.Contains(found[0].message, fmt.Sprint(maxFailureDomains+1)) {
		t.Errorf("the finding does not say how many there would be: %s", found[0].message)
	}
}

func TestADocumentFillingTheMappingExactlyIsAccepted(t *testing.T) {
	config := documentWithDomainCount(maxFailureDomains, nil)
	r := reconcilerFor(t, config)

	if found := tooManyDomains(t, r, config); len(found) != 0 {
		t.Errorf("findings = %+v, want none at exactly the limit", found)
	}
}

// A growth document is counted against what the cluster has already mapped,
// since the mapping only grows. A label the cluster already holds costs nothing.
func TestAGrowthDocumentIsCountedAgainstTheClustersMapping(t *testing.T) {
	assigned := make(domains, 0, maxFailureDomains-1)
	for i := range maxFailureDomains - 1 {
		assigned = append(assigned, simplyblockv1alpha2.FailureDomainIndex{
			Name: fmt.Sprintf("old-%d", i), Index: int32(i),
		})
	}
	cluster := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.FailureDomains = assigned
	})
	growth := func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.ClusterRef = theCluster
		c.Spec.Cluster = nil
	}

	fits := documentWithDomainCount(1, growth)
	fits.Spec.NodeSets[0].Groups = append(fits.Spec.NodeSets[0].Groups,
		simplyblockv1alpha2.NodeGroup{
			Name: "existing", Workers: []string{"worker-x"}, FailureDomain: "old-0",
		})
	if found := tooManyDomains(t, reconcilerFor(t, fits, cluster), fits); len(found) != 0 {
		t.Errorf("findings = %+v, want none: one new label fills the mapping", found)
	}

	overflows := documentWithDomainCount(2, growth)
	if found := tooManyDomains(t, reconcilerFor(t, overflows, cluster.DeepCopy()), overflows); len(found) != 1 {
		t.Errorf("findings = %+v, want one TooManyFailureDomains", found)
	}
}
