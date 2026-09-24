// The document's SPDK image pin, and the node it has to reach.
//
// The field it lands in lives on StorageNode and nothing sets it: before the
// document could state one, pinning a build meant patching every node CR in the
// window between its creation and its first add, and losing that race meant
// stopping each node and restarting it with --spdk-image instead.

package deployment

import (
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The pin reaches every node the document creates.
func TestTheSpdkImagePinReachesTheNodes(t *testing.T) {
	const image = "public.ecr.aws/simply-block/ultra:a-build-under-test"
	const proxy = "docker.io/simplyblock/simplyblock:a-build-under-test"

	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.SpdkImage = image
		c.Spec.Cluster.SpdkProxyImage = proxy
	})
	r := reconcilerFor(t)
	cluster, err := r.buildCluster(config, theCluster)
	if err != nil {
		t.Fatalf("the document did not describe a cluster: %v", err)
	}

	set := config.Spec.NodeSets[0]
	group := set.Groups[0]
	for _, worker := range group.Workers {
		node := r.buildNode(config, cluster, set, group, worker, 0)
		if got := node.Spec.Config.SpdkImage; got != image {
			t.Errorf("%s got SPDK image %q, want the document's %q", worker, got, image)
		}
		if got := node.Spec.Config.SpdkProxyImage; got != proxy {
			t.Errorf("%s got proxy image %q, want the document's %q", worker, got, proxy)
		}
	}
}

// A document that states no pin leaves the node's fields empty, which is what the
// control plane reads as "use the image I would have chosen". Defaulting to
// anything here would take that choice away from every deployment that never
// asked to pin one.
func TestASilentDocumentPinsNoImage(t *testing.T) {
	config := aDocument(nil)
	r := reconcilerFor(t)
	cluster, err := r.buildCluster(config, theCluster)
	if err != nil {
		t.Fatalf("the document did not describe a cluster: %v", err)
	}

	set := config.Spec.NodeSets[0]
	group := set.Groups[0]
	node := r.buildNode(config, cluster, set, group, group.Workers[0], 0)

	if got := node.Spec.Config.SpdkImage; got != "" {
		t.Errorf("a document that pinned nothing produced SPDK image %q", got)
	}
	if got := node.Spec.Config.SpdkProxyImage; got != "" {
		t.Errorf("a document that pinned nothing produced proxy image %q", got)
	}
}

// A growth document carries no cluster template at all, and asking it for a pin
// must answer "none" rather than panic on the way there.
func TestADocumentWithNoClusterTemplatePinsNoImage(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster = nil
	})

	if got := spdkImageOf(config); got != "" {
		t.Errorf("a document with no cluster template produced SPDK image %q", got)
	}
	if got := spdkProxyImageOf(config); got != "" {
		t.Errorf("a document with no cluster template produced proxy image %q", got)
	}
}
