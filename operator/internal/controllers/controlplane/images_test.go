// Tests that the images the control-plane install deploys are mirrorable:
// pinned by digest where the consumer allows it, listed for mirroring, and
// moved to the mirror registry when one is configured.

package controlplane

import (
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Every control-plane image is in related-images.yaml, the list of images to mirror.
func TestControlPlaneImagesAreListedForTheBundle(t *testing.T) {
	raw, err := os.ReadFile("../../prerequisites/related-images.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{fdbOperatorImage, fdbMonitorImage, minioImage, minioClientImage} {
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("%s is not pinned by digest", ref)
		}
		if !strings.Contains(string(raw), ref) {
			t.Errorf("%s is missing from related-images.yaml", ref)
		}
	}
}

// With a mirror registry set, the FoundationDB operator, the database's base
// image, and the object store are all pulled from it.
func TestControlPlaneImagesFollowTheMirrorRegistry(t *testing.T) {
	SetImageMirrorRegistry("mirror.example:5000")
	t.Cleanup(func() { SetImageMirrorRegistry("") })
	cp := localControlPlane()
	const want = "mirror.example:5000/simplyblock-io/"

	if img := fdbOperatorDeployment(cp).Spec.Template.Spec.Containers[0].Image; !strings.HasPrefix(img, want) {
		t.Errorf("FoundationDB operator image = %s", img)
	}
	for _, c := range minioStatefulSet(cp).Spec.Template.Spec.Containers {
		if !strings.HasPrefix(c.Image, want) {
			t.Errorf("object store container %s image = %s", c.Name, c.Image)
		}
	}
	for _, c := range minioStatefulSet(cp).Spec.Template.Spec.InitContainers {
		if !strings.HasPrefix(c.Image, want) {
			t.Errorf("object store init container %s image = %s", c.Name, c.Image)
		}
	}
	configs, _, _ := unstructured.NestedSlice(foundationDBCluster(cp, "").Object, "spec", "mainContainer", "imageConfigs")
	if len(configs) == 0 || !strings.HasPrefix(configs[0].(map[string]any)["baseImage"].(string), want) {
		t.Errorf("FoundationDB baseImage = %v", configs)
	}
}

// The NUMA plugin, which this install also deploys, follows the mirror registry.
func TestTheNUMAPluginFollowsTheMirrorRegistry(t *testing.T) {
	SetImageMirrorRegistry("mirror.example:5000")
	t.Cleanup(func() { SetImageMirrorRegistry("") })
	r := &ControlPlaneReconciler{NUMAPluginImage: "quay.io/simplyblock-io/numa-resource-plugin:v1@sha256:" + strings.Repeat("a", 64)}
	objs, err := r.numaPluginObjects(localControlPlane())
	if err != nil {
		t.Fatal(err)
	}
	ds := objs[1].(*appsv1.DaemonSet)
	if img := ds.Spec.Template.Spec.Containers[0].Image; !strings.HasPrefix(img, "mirror.example:5000/simplyblock-io/numa-resource-plugin:v1@sha256:") {
		t.Errorf("NUMA plugin image = %s", img)
	}
}
