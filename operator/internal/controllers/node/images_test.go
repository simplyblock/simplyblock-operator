// Which SPDK images a node should run, whether it runs them, and that every
// restart the operator asks for carries them. Without the last part a restart
// starts a node on the image it was added with, and a rolling restart after an
// upgrade leaves the whole fleet on the old one (design-release-upgrade.md §5).

package node

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/simplyblock/atlas/crdtypes"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	nodeSpdkImage       = "quay.io/simplyblock-io/ultra:26.5.0-node"
	clusterSpdkImage    = "quay.io/simplyblock-io/ultra:26.5.0-cluster"
	defaultSpdkImage    = "quay.io/simplyblock-io/ultra:26.5.0"
	defaultProxyImage   = "quay.io/simplyblock-io/simplyblock:26.5.0"
	oldSpdkImage        = "quay.io/simplyblock-io/ultra:26.4.0-beta6.1"
	oldSpdkProxyImage   = "quay.io/simplyblock-io/simplyblock:26.4.0-beta6.1"
	clusterProxyImage   = "quay.io/simplyblock-io/simplyblock:26.5.0-cluster"
	desiredSpdkImage    = defaultSpdkImage
	desiredSpdkProxyImg = defaultProxyImage
)

func imagesCluster() *simplyblockv1alpha2.StorageCluster {
	cluster := &simplyblockv1alpha2.StorageCluster{}
	cluster.Status.DefaultSpdkImage = defaultSpdkImage
	cluster.Status.DefaultSpdkProxyImage = defaultProxyImage
	return cluster
}

func TestTheDesiredImageIsTheNodesThenTheClustersThenTheDefault(t *testing.T) {
	for _, tc := range []struct {
		name               string
		nodeImage, cluster string
		want               string
	}{
		{"the node's own", nodeSpdkImage, clusterSpdkImage, nodeSpdkImage},
		{"the cluster's", "", clusterSpdkImage, clusterSpdkImage},
		{"the control plane's default", "", "", defaultSpdkImage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &simplyblockv1alpha2.StorageNode{}
			node.Spec.Config.SpdkImage = tc.nodeImage
			cluster := imagesCluster()
			cluster.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{}
			cluster.Spec.StorageNodes.SpdkImage = simplyblockImage(tc.cluster)

			status := &simplyblockv1alpha2.StorageNodeStatus{}
			applyImages(status, node, cluster, NodeReading{SpdkImage: tc.want})
			if status.DesiredSpdkImage != tc.want {
				t.Errorf("desiredSpdkImage = %q, want %q", status.DesiredSpdkImage, tc.want)
			}
		})
	}
}

func TestTheProxyImageResolvesOnItsOwn(t *testing.T) {
	node := &simplyblockv1alpha2.StorageNode{}
	node.Spec.Config.SpdkImage = nodeSpdkImage
	cluster := imagesCluster()
	cluster.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{SpdkProxyImage: simplyblockImage(clusterProxyImage)}

	status := &simplyblockv1alpha2.StorageNodeStatus{}
	applyImages(status, node, cluster, NodeReading{})
	if status.DesiredSpdkImage != nodeSpdkImage || status.DesiredSpdkProxyImage != clusterProxyImage {
		t.Errorf("desired = %q, %q, want the node's SPDK image and the cluster's proxy image",
			status.DesiredSpdkImage, status.DesiredSpdkProxyImage)
	}
}

func TestNothingAskedForLeavesTheDesiredImagesEmpty(t *testing.T) {
	status := &simplyblockv1alpha2.StorageNodeStatus{}
	applyImages(status, &simplyblockv1alpha2.StorageNode{}, &simplyblockv1alpha2.StorageCluster{},
		NodeReading{SpdkImage: oldSpdkImage, SpdkProxyImage: oldSpdkProxyImage})
	if status.DesiredSpdkImage != "" || status.DesiredSpdkProxyImage != "" {
		t.Errorf("desired = %q, %q, want none", status.DesiredSpdkImage, status.DesiredSpdkProxyImage)
	}
	if status.ImagesCurrent == nil || !*status.ImagesCurrent {
		t.Errorf("imagesCurrent = %v, want true: no image is asked for", status.ImagesCurrent)
	}
}

func TestANodeOnItsDesiredImagesIsCurrent(t *testing.T) {
	status := &simplyblockv1alpha2.StorageNodeStatus{}
	became := applyImages(status, &simplyblockv1alpha2.StorageNode{}, imagesCluster(),
		NodeReading{SpdkImage: defaultSpdkImage, SpdkProxyImage: defaultProxyImage})

	if status.SpdkImage != defaultSpdkImage || status.SpdkProxyImage != defaultProxyImage {
		t.Errorf("reported images = %q, %q, want the reading's", status.SpdkImage, status.SpdkProxyImage)
	}
	if status.ImagesCurrent == nil || !*status.ImagesCurrent {
		t.Errorf("imagesCurrent = %v, want true", status.ImagesCurrent)
	}
	if became {
		t.Error("a current node was reported as having become out of date")
	}
}

func TestANodeOnOtherImagesIsOutOfDateOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		spdk, proxy string
	}{
		{"the SPDK image differs", oldSpdkImage, defaultProxyImage},
		{"only the proxy image differs", defaultSpdkImage, oldSpdkProxyImage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := &simplyblockv1alpha2.StorageNodeStatus{ImagesCurrent: ptr.To(true)}
			reading := NodeReading{SpdkImage: tc.spdk, SpdkProxyImage: tc.proxy}

			if !applyImages(status, &simplyblockv1alpha2.StorageNode{}, imagesCluster(), reading) {
				t.Error("the node going out of date was not reported")
			}
			if status.ImagesCurrent == nil || *status.ImagesCurrent {
				t.Errorf("imagesCurrent = %v, want false", status.ImagesCurrent)
			}
			if applyImages(status, &simplyblockv1alpha2.StorageNode{}, imagesCluster(), reading) {
				t.Error("a node already out of date was reported again")
			}
		})
	}
}

func TestANodeWhoseImagesAreNotReportedIsUnknown(t *testing.T) {
	status := &simplyblockv1alpha2.StorageNodeStatus{}
	if applyImages(status, &simplyblockv1alpha2.StorageNode{}, imagesCluster(), NodeReading{}) {
		t.Error("an unknown node was reported as out of date")
	}
	if status.ImagesCurrent != nil {
		t.Errorf("imagesCurrent = %v, want unset", *status.ImagesCurrent)
	}
	if status.DesiredSpdkImage != defaultSpdkImage {
		t.Errorf("desiredSpdkImage = %q, want it resolved all the same", status.DesiredSpdkImage)
	}
}

func TestTheStreamedNodeCarriesItsImages(t *testing.T) {
	reading := readingFromDTO(subscriptions.NodeDTO{SpdkImage: oldSpdkImage, SpdkProxyImage: oldSpdkProxyImage})
	if reading.SpdkImage != oldSpdkImage || reading.SpdkProxyImage != oldSpdkProxyImage {
		t.Errorf("reading images = %q, %q, want the streamed ones", reading.SpdkImage, reading.SpdkProxyImage)
	}
}

// desiringImages records desired images on the operations' node, as its
// reconciler does.
func desiringImages(t *testing.T, c client.Client) {
	t.Helper()
	var node simplyblockv1alpha2.StorageNode
	if err := c.Get(context.Background(), types.NamespacedName{Name: opsNodeName, Namespace: opsNamespace}, &node); err != nil {
		t.Fatal(err)
	}
	node.Status.DesiredSpdkImage = desiredSpdkImage
	node.Status.DesiredSpdkProxyImage = desiredSpdkProxyImg
	if err := c.Status().Update(context.Background(), &node); err != nil {
		t.Fatal(err)
	}
}

func assertRestartCarriesTheDesiredImages(t *testing.T, restarts []RestartParams) {
	t.Helper()
	if len(restarts) != 1 {
		t.Fatalf("%d restarts were issued, want one", len(restarts))
	}
	if restarts[0].SpdkImage != desiredSpdkImage || restarts[0].SpdkProxyImage != desiredSpdkProxyImg {
		t.Errorf("the restart carries %q, %q, want the node's desired images",
			restarts[0].SpdkImage, restarts[0].SpdkProxyImage)
	}
}

func TestARestartOperationCarriesTheDesiredImages(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusOffline)
	r, c := anOpsWorld(t, api)
	desiringImages(t, c)

	if _, err := performing(t, r, anOperation("a-restart", simplyblockv1alpha2.StorageNodeOpsActionRestart),
		stepRequesting); err != nil {
		t.Fatalf("the restart request: %v", err)
	}
	assertRestartCarriesTheDesiredImages(t, api.restarts)
}

func TestTheRestartAfterMaintenanceCarriesTheDesiredImages(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusOffline)
	r, c := anOpsWorld(t, api)
	desiringImages(t, c)

	if _, err := performing(t, r, aWindow("a-window"), stepRestarting); err != nil {
		t.Fatalf("restarting: %v", err)
	}
	assertRestartCarriesTheDesiredImages(t, api.restarts)
}

func TestTheRelocationRestartCarriesTheDesiredImages(t *testing.T) {
	api := aControlPlane()
	r, c := anOpsWorld(t, api, aWorker(opsTarget, true))
	desiringImages(t, c)

	if _, err := performing(t, r, aRelocation(), stepRelocating); err != nil {
		t.Fatalf("relocating: %v", err)
	}
	assertRestartCarriesTheDesiredImages(t, api.restarts)
}

func TestARestartWithoutDesiredImagesNamesNone(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusOffline)
	r, _ := anOpsWorld(t, api)

	if _, err := performing(t, r, anOperation("a-restart", simplyblockv1alpha2.StorageNodeOpsActionRestart),
		stepRequesting); err != nil {
		t.Fatalf("the restart request: %v", err)
	}
	if len(api.restarts) != 1 || api.restarts[0].SpdkImage != "" || api.restarts[0].SpdkProxyImage != "" {
		t.Errorf("restarts = %+v, want one naming no image", api.restarts)
	}
}

func simplyblockImage(image string) crdtypes.ImageReference { return crdtypes.ImageReference(image) }

// The gauge a fleet left half upgraded is alerted on: one per node, 1 while the
// node runs images other than its desired ones (design-release-upgrade.md §12).
func TestTheOutOfDateGaugeFollowsTheNode(t *testing.T) {
	node := &simplyblockv1alpha2.StorageNode{}
	node.Name, node.Spec.ClusterRef = "n-images", "c-images"

	for _, tc := range []struct {
		current *bool
		want    float64
	}{
		{ptr.To(false), 1},
		{ptr.To(true), 0},
		{nil, 0},
	} {
		node.Status.ImagesCurrent = tc.current
		observeImages(node)
		if got := testutil.ToFloat64(imagesOutOfDate.WithLabelValues("c-images", "n-images")); got != tc.want {
			t.Errorf("imagesCurrent %v: gauge = %v, want %v", tc.current, got, tc.want)
		}
	}
}
