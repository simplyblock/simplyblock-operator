// What a document's erasure coding is answered against: the scheme the control
// plane would accept, and the storage nodes the deployment actually produces.
//
// The cases are written against validate() rather than against the counting
// helpers, because the thing being guaranteed is that a reviewer is told before
// approval, and a helper that counts correctly while nothing calls it
// guarantees nothing.

package deployment

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aNode is a storage node the cluster already has, which is what a growth
// document adds to. Every case puts its node in the first slot, because what
// the cases differ in is how many nodes there are rather than where they sit.
func aNode(name, worker string) client.Object {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: theNamespace},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: theCluster,
			WorkerNode: worker,
			Slot:       ptr.To(int32(0)),
		},
	}
}

// findingsOf validates a draft against the objects the cluster carries.
func findingsOf(
	t *testing.T,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	objects ...client.Object,
) []finding {
	t.Helper()
	r := reconcilerFor(t, append(objects, config)...)
	findings, err := r.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return findings
}

// only asserts one finding under one reason and returns it, because a document
// with two problems reported as one is a document whose second problem is
// discovered after the first is fixed.
func only(t *testing.T, findings []finding, reason string) finding {
	t.Helper()
	if len(findings) != 1 || findings[0].reason != reason {
		t.Fatalf("findings = %+v, want one %s", findings, reason)
	}
	return findings[0]
}

// A document that says nothing about erasure coding describes a 1+1 cluster,
// because that is what the control plane defaults to, and 1+1 needs three
// storage nodes. Two workers cannot carry it, and saying so while the document
// is a draft is the only moment it can be corrected.
func TestADraftTooSmallForTheDefaultStripeIsReported(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.Cluster.Stripe = nil
	})

	found := only(t, findingsOf(t, config, workers("worker-1", "worker-2")...),
		StripeBelowMinimumNodes)
	for _, want := range []string{"1+1", "3", "2"} {
		if !strings.Contains(found.message, want) {
			t.Errorf("the finding does not mention %q: %s", want, found.message)
		}
	}
}

// A scheme the control plane's supported set does not hold is refused here,
// where the document can still be edited, rather than by the cluster create
// that runs after approval has made it immutable.
func TestADraftWhoseSchemeTheControlPlaneRefusesIsReported(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.Cluster.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(3)), ParityChunks: ptr.To(int32(1)),
		}
	})

	found := only(t, findingsOf(t, config, workers("worker-1", "worker-2")...),
		StripeUnsupported)
	for _, want := range []string{"3+1", "2+1"} {
		if !strings.Contains(found.message, want) {
			t.Errorf("the finding does not mention %q: %s", want, found.message)
		}
	}
}

// The minimum of a scheme that does not exist is not a fact, so an unsupported
// scheme is reported once rather than followed by a node count derived from it.
func TestAnUnsupportedSchemeIsReportedWithoutItsNodeCount(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.Cluster.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(4)), ParityChunks: ptr.To(int32(3)),
		}
	})

	only(t, findingsOf(t, config, workers("worker-1", "worker-2")...), StripeUnsupported)
}

// 1+0 is the one scheme a fleet of two carries, and a document stating it is
// accepted: the guarantee is that a deployment cannot fall below its stripe's
// minimum, not that every deployment is redundant.
func TestADraftStatingNoRedundancyIsAccepted(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
	})

	if findings := findingsOf(t, config, workers("worker-1", "worker-2")...); len(findings) != 0 {
		t.Errorf("a 1+0 document on two workers reported %+v", findings)
	}
}

// Two storage nodes on one worker die with the worker, so a fleet that reaches
// the node count only by running several nodes per socket has not reached the
// spare the scheme requires.
func TestNodesSharingAWorkerAreNotSpares(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.Cluster.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(1)),
		}
		c.Spec.Cluster.NodesPerSocket = ptr.To(int32(2))
	})

	found := only(t, findingsOf(t, config, workers("worker-1", "worker-2")...),
		StripeBelowMinimumWorkers)
	if !strings.Contains(found.message, "worker") {
		t.Errorf("the finding does not say what is short: %s", found.message)
	}
}

// A growth document is answered against the cluster it grows, so the nodes that
// are already there count toward the minimum.
func TestAGrowthDocumentCountsTheNodesTheClusterHas(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.ClusterRef = theCluster
		c.Spec.Cluster = nil
		c.Spec.NodeSets[0].Groups[0].Workers = []string{"worker-3", "worker-4"}
	})
	cluster := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(2)), ParityChunks: ptr.To(int32(1)),
		}
	})
	objects := append(workers("worker-1", "worker-2", "worker-3", "worker-4"),
		cluster, aNode("node-1", "worker-1"), aNode("node-2", "worker-2"))

	if findings := findingsOf(t, config, objects...); len(findings) != 0 {
		t.Errorf("a growth document reaching the minimum reported %+v", findings)
	}
}

// The same document against a cluster that has one node is still short, and the
// message counts what the cluster would end up with rather than what the
// document adds.
func TestAGrowthDocumentThatStaysBelowTheMinimumIsReported(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.ClusterRef = theCluster
		c.Spec.Cluster = nil
		c.Spec.NodeSets[0].Groups[0].Workers = []string{"worker-3"}
	})
	cluster := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(2)), ParityChunks: ptr.To(int32(1)),
		}
	})
	objects := append(workers("worker-1", "worker-3"),
		cluster, aNode("node-1", "worker-1"))

	found := only(t, findingsOf(t, config, objects...), StripeBelowMinimumNodes)
	for _, want := range []string{"2+1", "4", "2"} {
		if !strings.Contains(found.message, want) {
			t.Errorf("the finding does not mention %q: %s", want, found.message)
		}
	}
}

// A growth document naming a worker the cluster already has a node on adds
// nothing there, because a node is identified by its worker and slot and the
// expansion creates only the slots that are empty. Counting it twice would
// report a minimum as met by a node that does not exist.
func TestAGrowthDocumentDoesNotCountASlotTwice(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.ClusterRef = theCluster
		c.Spec.Cluster = nil
		c.Spec.NodeSets[0].Groups[0].Workers = []string{"worker-1", "worker-2"}
	})
	cluster := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(1)),
		}
	})
	objects := append(workers("worker-1", "worker-2"),
		cluster, aNode("node-1", "worker-1"), aNode("node-2", "worker-2"))

	found := only(t, findingsOf(t, config, objects...), StripeBelowMinimumNodes)
	if !strings.Contains(found.message, "2") {
		t.Errorf("the finding counts the re-named slots twice: %s", found.message)
	}
}
