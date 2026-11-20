// Tests that keep the add-on images mirrorable: pinned by digest, listed for
// mirroring, and rewritable to a mirror registry.

package prerequisites

import (
	"context"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

var imageLine = regexp.MustCompile(`(?:image: "?|-image=)([^"\s]+)`)

// Every image a manifest names is fully qualified and pinned by digest, which
// is what an IDMS mirror rule and oc-mirror both key on.
func TestEveryImageIsPinnedByDigest(t *testing.T) {
	for _, p := range All() {
		raw, err := manifests.ReadFile(p.file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range imageLine.FindAllStringSubmatch(string(raw), -1) {
			ref := m[1]
			if !strings.Contains(ref, "@sha256:") || !strings.Contains(strings.SplitN(ref, "/", 2)[0], ".") {
				t.Errorf("%s: %s is not a fully qualified, digest-pinned reference", p.Name, ref)
			}
		}
	}
}

// related-images.yaml, the list of images to mirror, names every image
// the manifests use.
func TestRelatedImagesMatchTheManifests(t *testing.T) {
	raw, err := os.ReadFile("related-images.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var listed []struct{ Name, Image string }
	if err := yaml.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(listed))
	for _, l := range listed {
		got = append(got, l.Image)
	}
	for _, want := range Images() {
		if !slices.Contains(got, want) {
			t.Errorf("%s is used by a manifest but missing from related-images.yaml", want)
		}
	}
}

// With a mirror registry set, every image is rewritten to it, path and digest
// kept, including the helper image inside the local-path ConfigMap.
func TestImagesAreRewrittenToTheMirrorRegistry(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	opts := InstallOptions{ImageRegistry: "mirror.example:5000", Namespace: "simplyblock"}
	if err := LocalPathProvisioner.Install(ctx, c, opts); err != nil {
		t.Fatal(err)
	}
	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: "simplyblock", Name: "local-path-provisioner"}, &d); err != nil {
		t.Fatal(err)
	}
	want := "mirror.example:5000/simplyblock-io/local-path-provisioner:v0.0.30@sha256:"
	if img := d.Spec.Template.Spec.Containers[0].Image; !strings.HasPrefix(img, want) {
		t.Errorf("provisioner image = %s, want prefix %s", img, want)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: "simplyblock", Name: "local-path-config"}, &cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["helperPod.yaml"], "mirror.example:5000/simplyblock-io/busybox:1.37.0@sha256:") {
		t.Errorf("helper pod image not rewritten:\n%s", cm.Data["helperPod.yaml"])
	}
}
