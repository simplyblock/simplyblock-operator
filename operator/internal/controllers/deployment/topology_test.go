// The topology rules approval holds every document to (design-superclusters.md
// §5.7): a node set stays within one region, and with failure domains enabled
// every node set of a region holds every domain the region has.

package deployment

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// inRegion is a worker carrying the region label, or none when region is empty.
func inRegion(name, region string) client.Object {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if region != "" {
		node.Labels = map[string]string{corev1.LabelTopologyRegion: region}
	}
	return node
}

// findingOf is the first finding under the reason, or none.
func findingOf(findings []finding, reason string) *finding {
	for i := range findings {
		if findings[i].reason == reason {
			return &findings[i]
		}
	}
	return nil
}

// A member stripes every write across its own nodes, so one spanning two
// regions puts inter-region latency in every write.
func TestANodeSetSpanningTwoRegionsIsRefused(t *testing.T) {
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.NodeSets[1] = aNodeSet("dense", "eth3", "worker-2", "worker-3")
	})
	objects := []client.Object{inRegion("worker-1", "eu-west"), inRegion("worker-2", "eu-west"), inRegion("worker-3", "us-east")}

	found := findingOf(findingsOf(t, config, objects...), MemberSpansRegions)
	if found == nil || !strings.Contains(found.message, "dense") ||
		!strings.Contains(found.message, "eu-west") || !strings.Contains(found.message, "us-east") {
		t.Errorf("MemberSpansRegions = %+v, want it to name node set dense and both regions", found)
	}
}

// A worker without the region label matches any region, so a fleet with no
// region labels is never refused for it.
func TestAWorkerWithoutARegionMatchesAny(t *testing.T) {
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.NodeSets[1] = aNodeSet("dense", "eth3", "worker-2", "worker-3")
	})
	objects := []client.Object{inRegion("worker-1", ""), inRegion("worker-2", "eu-west"), inRegion("worker-3", "")}

	if found := findingOf(findingsOf(t, config, objects...), MemberSpansRegions); found != nil {
		t.Errorf("MemberSpansRegions = %+v, want none", found)
	}
}

// withDomains gives a node set one group per failure domain, one worker each.
func withDomains(name string, domains map[string]string) simplyblockv1alpha2.NodeSet {
	set := simplyblockv1alpha2.NodeSet{Name: name}
	for worker, domain := range domains {
		group := aNodeSet(name, "eth1", worker).Groups[0]
		group.Name = "group-" + worker
		group.FailureDomain = domain
		set.Groups = append(set.Groups, group)
	}
	return set
}

// With failure domains enabled, a domain's loss takes nodes from every member
// of its region and all of no member, so a member missing a domain another
// member of its region holds is refused.
func TestAMemberMissingAFailureDomainOfItsRegionIsRefused(t *testing.T) {
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.EnableFailureDomains = ptr.To(true)
		c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
			withDomains("fast", map[string]string{"worker-1": "zone-a", "worker-2": "zone-b"}),
			withDomains("dense", map[string]string{"worker-3": "zone-a"}),
		}
	})
	objects := workers("worker-1", "worker-2", "worker-3")

	found := findingOf(findingsOf(t, config, objects...), MemberMissingFailureDomain)
	if found == nil || !strings.Contains(found.message, "dense") || !strings.Contains(found.message, "zone-b") {
		t.Errorf("MemberMissingFailureDomain = %+v, want it to name node set dense and zone-b", found)
	}
}

// Members of two regions hold the domains of their own region only.
func TestMembersOfTwoRegionsHoldTheirOwnRegionsDomains(t *testing.T) {
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.EnableFailureDomains = ptr.To(true)
		c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
			withDomains("eu", map[string]string{"worker-1": "eu-a"}),
			withDomains("us", map[string]string{"worker-2": "us-a"}),
		}
	})
	objects := []client.Object{inRegion("worker-1", "eu-west"), inRegion("worker-2", "us-east")}

	if found := findingOf(findingsOf(t, config, objects...), MemberMissingFailureDomain); found != nil {
		t.Errorf("MemberMissingFailureDomain = %+v, want none", found)
	}
}

// Without failure domains, the domains a document names hold nothing, so node
// sets naming different ones are admitted.
func TestFailureDomainsDisabledAdmitsDifferentDomains(t *testing.T) {
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
			withDomains("fast", map[string]string{"worker-1": "zone-a", "worker-2": "zone-b"}),
			withDomains("dense", map[string]string{"worker-3": "zone-a"}),
		}
	})

	if found := findingOf(findingsOf(t, config, workers("worker-1", "worker-2", "worker-3")...), MemberMissingFailureDomain); found != nil {
		t.Errorf("MemberMissingFailureDomain = %+v, want none", found)
	}
}
