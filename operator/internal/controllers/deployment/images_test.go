// What spec.images reaches. The document states each image once, and the
// expansion spends the three slots on two different objects: the storage-node
// workload is the cluster's, and the two SPDK images are every node's.
//
// The reason to test the spending rather than the field is that the split is the
// part that can be got wrong. A policy written onto the cluster instead of the
// nodes is a document that reads correctly and deploys the wrong thing.

package deployment

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	theSpdkImage    = "public.ecr.aws/simply-block/ultra:main-latest"
	theProxyImage   = "public.ecr.aws/simply-block/simplyblock:main"
	theClusterImage = "public.ecr.aws/simply-block/simplyblock-operator:initialize-indices"
)

// withImages is the document fixture with all three slots stated, which is what
// an air-gapped deployment writes.
func withImages(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
	c.Spec.Images = &simplyblockv1alpha2.DeploymentImages{
		Cluster: &simplyblockv1alpha2.ImageSpec{
			Image:           theClusterImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
		},
		SPDK: &simplyblockv1alpha2.ImageSpec{
			Image:           theSpdkImage,
			ImagePullPolicy: corev1.PullAlways,
		},
		SPDKProxy: &simplyblockv1alpha2.ImageSpec{
			Image:           theProxyImage,
			ImagePullPolicy: corev1.PullNever,
		},
	}
}

// The cluster slot is the storage-node workload's image, which is where the
// v1alpha1 clusterImage went.
func TestTheClusterImageReachesTheWorkload(t *testing.T) {
	config := aDocument(withImages)
	r := reconcilerFor(t)

	workload := r.buildWorkload(config)
	if workload.Image != theClusterImage {
		t.Errorf("the workload runs %q, want the document's %q", workload.Image, theClusterImage)
	}
	if workload.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("the workload pulls %q, want the document's %q",
			workload.ImagePullPolicy, corev1.PullIfNotPresent)
	}
}

// The two SPDK slots are every node's, because the fields they land on are per
// node so that a later rollout can walk the fleet one machine at a time. A
// document states them once and the expansion writes them onto each node it
// creates.
func TestTheSPDKImagesReachEveryNode(t *testing.T) {
	config := aDocument(withImages)
	cluster := aCluster(nil)
	r := reconcilerFor(t)

	node := r.buildNode(config, cluster,
		config.Spec.NodeSets[0], config.Spec.NodeSets[0].Groups[0], "worker-1", 0)

	got := node.Spec.Config
	if got.SpdkImage != theSpdkImage {
		t.Errorf("the node runs SPDK %q, want %q", got.SpdkImage, theSpdkImage)
	}
	if got.SpdkImagePullPolicy != corev1.PullAlways {
		t.Errorf("the node pulls SPDK %q, want %q", got.SpdkImagePullPolicy, corev1.PullAlways)
	}
	if got.SpdkProxyImage != theProxyImage {
		t.Errorf("the node runs the proxy %q, want %q", got.SpdkProxyImage, theProxyImage)
	}
	if got.SpdkProxyImagePullPolicy != corev1.PullNever {
		t.Errorf("the node pulls the proxy %q, want %q",
			got.SpdkProxyImagePullPolicy, corev1.PullNever)
	}
}

// A document that states no images states nothing downstream either, so each
// field's own default decides. Writing an empty string and an empty policy would
// be the same as stating them, and would override a cluster's image with nothing.
func TestADocumentWithNoImagesStatesNone(t *testing.T) {
	config := aDocument(nil)
	cluster := aCluster(nil)
	r := reconcilerFor(t)

	workload := r.buildWorkload(config)
	if workload.Image != "" || workload.ImagePullPolicy != "" {
		t.Errorf("an unstated cluster image reached the workload as %q/%q",
			workload.Image, workload.ImagePullPolicy)
	}

	got := r.buildNode(config, cluster,
		config.Spec.NodeSets[0], config.Spec.NodeSets[0].Groups[0], "worker-1", 0).Spec.Config
	if got.SpdkImage != "" || got.SpdkImagePullPolicy != "" ||
		got.SpdkProxyImage != "" || got.SpdkProxyImagePullPolicy != "" {
		t.Errorf("unstated SPDK images reached the node as %q/%q and %q/%q",
			got.SpdkImage, got.SpdkImagePullPolicy,
			got.SpdkProxyImage, got.SpdkProxyImagePullPolicy)
	}
}

// One slot stated and the others absent is the common case: an override of the
// SPDK image alone, with the rest left to the defaults.
func TestOneStatedSlotLeavesTheOthersUnstated(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Images = &simplyblockv1alpha2.DeploymentImages{
			SPDK: &simplyblockv1alpha2.ImageSpec{Image: theSpdkImage},
		}
	})
	cluster := aCluster(nil)
	r := reconcilerFor(t)

	if workload := r.buildWorkload(config); workload.Image != "" {
		t.Errorf("an unstated cluster slot reached the workload as %q", workload.Image)
	}

	got := r.buildNode(config, cluster,
		config.Spec.NodeSets[0], config.Spec.NodeSets[0].Groups[0], "worker-1", 0).Spec.Config
	if got.SpdkImage != theSpdkImage {
		t.Errorf("the stated SPDK image did not reach the node: %q", got.SpdkImage)
	}
	if got.SpdkProxyImage != "" {
		t.Errorf("an unstated proxy slot reached the node as %q", got.SpdkProxyImage)
	}
}

// What the apiserver itself does with spec.images: the policy it stamps on a
// slot that states only an image, and the two values it refuses. A schema
// default and a pattern are the apiserver's work, so a fake client would report
// them passing whatever the markers said.
func TestTheApiserverStampsAndPolicesTheImageSlots(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()

	t.Run("a slot with no policy is stamped Always", func(t *testing.T) {
		config := aStoredDocument(t, apiClient, "images-default")
		config.Spec.Images = &simplyblockv1alpha2.DeploymentImages{
			SPDK: &simplyblockv1alpha2.ImageSpec{Image: theSpdkImage},
		}
		if err := apiClient.Update(ctx, config); err != nil {
			t.Fatalf("stating one image: %v", err)
		}
		if got := config.Spec.Images.SPDK.ImagePullPolicy; got != corev1.PullAlways {
			t.Errorf("a slot that named no policy came back with %q, want %q",
				got, corev1.PullAlways)
		}
	})

	t.Run("a policy outside the enum is refused", func(t *testing.T) {
		config := aStoredDocument(t, apiClient, "images-bad-policy")
		config.Spec.Images = &simplyblockv1alpha2.DeploymentImages{
			SPDKProxy: &simplyblockv1alpha2.ImageSpec{ImagePullPolicy: "Sometimes"},
		}
		err := apiClient.Update(ctx, config)
		if err == nil {
			t.Fatal("a pull policy of Sometimes was accepted")
		}
		if !strings.Contains(err.Error(), "Unsupported value") {
			t.Errorf("refused for the wrong reason: %v", err)
		}
	})

	t.Run("an image outside the trusted registries is refused", func(t *testing.T) {
		config := aStoredDocument(t, apiClient, "images-bad-registry")
		config.Spec.Images = &simplyblockv1alpha2.DeploymentImages{
			Cluster: &simplyblockv1alpha2.ImageSpec{Image: "docker.io/someone/else:latest"},
		}
		err := apiClient.Update(ctx, config)
		if err == nil {
			t.Fatal("an image from an untrusted registry was accepted")
		}
		if !strings.Contains(err.Error(), "should match") {
			t.Errorf("refused for the wrong reason: %v", err)
		}
	})
}
