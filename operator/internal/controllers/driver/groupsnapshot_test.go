// The csi-snapshotter sidecar watches VolumeGroupSnapshotClass at exactly one
// version of groupsnapshot.storage.k8s.io, fixed by its release: 8.2 watches
// v1beta1, 8.4 v1beta2, 8.6 and later v1. A sidecar whose version the cluster
// does not serve fails that watch and logs it forever (GitHub #978, OpenShift
// 5, which serves v1 only), and group snapshots do not work. The operator
// therefore runs the release that matches the version the cluster serves, and
// turns the gate off where it serves none.

package driver

import (
	"errors"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/utils/ptr"

	groupsnapshotapi "github.com/simplyblock/simplyblock-operator/internal/groupsnapshot"
)

const (
	groupGateOn  = "--feature-gates=CSIVolumeGroupSnapshot=true"
	groupGateOff = "--feature-gates=CSIVolumeGroupSnapshot=false"
)

// versionedDiscovery serves the snapshot group and the given versions of the
// group snapshot group, and counts the questions.
type versionedDiscovery struct {
	discovery.DiscoveryInterface
	groupVersions map[string]bool
	err           error
	asked         int
}

func (v *versionedDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	v.asked++
	if v.err != nil {
		return nil, v.err
	}
	parsed, _ := schema.ParseGroupVersion(gv)
	if parsed.Group == groupsnapshotapi.Group && !v.groupVersions[parsed.Version] {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: parsed.Group}, "")
	}
	return &metav1.APIResourceList{}, nil
}

func TestTheNewestServedGroupSnapshotVersionIsReported(t *testing.T) {
	for _, tc := range []struct {
		served []string
		want   string
	}{
		{served: []string{"v1"}, want: "v1"},
		{served: []string{"v1beta1", "v1beta2", "v1"}, want: "v1"},
		{served: []string{"v1beta1", "v1beta2"}, want: "v1beta2"},
		{served: []string{"v1beta1"}, want: "v1beta1"},
		{served: nil, want: ""},
	} {
		served := map[string]bool{}
		for _, v := range tc.served {
			served[v] = true
		}
		api := &DiscoveredSnapshotAPI{Discovery: &versionedDiscovery{groupVersions: served}}

		got, err := api.GroupSnapshotAPIVersion(t.Context())
		if err != nil {
			t.Fatalf("served %v: %v", tc.served, err)
		}
		if got != tc.want {
			t.Errorf("served %v: version = %q, want %q", tc.served, got, tc.want)
		}
	}
}

// As for the snapshot API: an unreachable API server is not a cluster without
// group snapshots, and the reconcile refuses rather than turning them off.
func TestAGroupSnapshotDiscoveryFailureIsReported(t *testing.T) {
	api := &DiscoveredSnapshotAPI{Discovery: &versionedDiscovery{err: errors.New("connection refused")}}
	if _, err := api.GroupSnapshotAPIVersion(t.Context()); err == nil {
		t.Error("an unreachable API server was read as an answer about the group snapshot API")
	}
}

func TestTheGroupSnapshotVersionIsCached(t *testing.T) {
	discovered := &versionedDiscovery{groupVersions: map[string]bool{"v1": true}}
	api := &DiscoveredSnapshotAPI{Discovery: discovered}
	for range 3 {
		if _, err := api.GroupSnapshotAPIVersion(t.Context()); err != nil {
			t.Fatalf("asking: %v", err)
		}
	}
	if discovered.asked != 1 {
		t.Errorf("asked the API server %d times within the window, want once", discovered.asked)
	}
}

func snapshotterOf(t *testing.T, sts *appsv1.StatefulSet) (image string, args []string) {
	t.Helper()
	c := containerNamed(sts.Spec.Template.Spec.Containers, "csi-snapshotter")
	if c == nil {
		t.Fatal("no csi-snapshotter sidecar")
	}
	return c.Image, c.Args
}

func TestTheSnapshotterMatchesTheServedGroupSnapshotVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		image   string
		gate    string
	}{
		{version: "v1", image: snapshotterImageV1, gate: groupGateOn},
		{version: "v1beta2", image: snapshotterImageV1beta2, gate: groupGateOn},
		{version: "v1beta1", image: defaultSnapshotterImage, gate: groupGateOn},
		// No group snapshot API: the sidecar still serves VolumeSnapshots, with
		// the group watch off so it does not fail and log on every retry.
		{version: "", image: defaultSnapshotterImage, gate: groupGateOff},
	} {
		d := testDriver("simplyblock")
		image, args := snapshotterOf(t, withGroupSnapshotAPI(controllerStatefulSet(d, testImage), d, tc.version))
		if image != tc.image {
			t.Errorf("%q: image = %q, want %q", tc.version, image, tc.image)
		}
		if !slices.Contains(args, tc.gate) {
			t.Errorf("%q: args %v carry no %s", tc.version, args, tc.gate)
		}
		if slices.Contains(args, groupGateOn) && slices.Contains(args, groupGateOff) {
			t.Errorf("%q: args %v carry the gate twice", tc.version, args)
		}
	}
}

// An image the deployment chose is run as chosen: which version it watches is
// the deployment's to know. The gate still follows the cluster, so a cluster
// with no group snapshot API is not asked to watch one.
func TestAnOverriddenSnapshotterIsKept(t *testing.T) {
	for _, tc := range []struct {
		version string
		gate    string
	}{
		{version: "v1", gate: groupGateOn},
		{version: "", gate: groupGateOff},
	} {
		d := testDriver("simplyblock")
		d.Spec.SidecarImages.Snapshotter = "registry.example.com/csi-snapshotter:custom"
		image, args := snapshotterOf(t, withGroupSnapshotAPI(controllerStatefulSet(d, testImage), d, tc.version))
		if image != "registry.example.com/csi-snapshotter:custom" {
			t.Errorf("%q: image = %q, want the override", tc.version, image)
		}
		if !slices.Contains(args, tc.gate) {
			t.Errorf("%q: args %v carry no %s", tc.version, args, tc.gate)
		}
	}
}

// The reconcile asks the cluster and builds the controller with the answer.
func TestTheControllerIsBuiltForTheServedGroupSnapshotVersion(t *testing.T) {
	d := testDriver("simplyblock")
	r := &SimplyblockDriverReconciler{Snapshots: fixedSnapshotAPI{served: true, groupVersion: "v1"}}

	objects, err := r.desired(t.Context(), d, testImage)
	if err != nil {
		t.Fatalf("building the object set: %v", err)
	}
	for _, obj := range objects {
		if sts, ok := obj.(*appsv1.StatefulSet); ok {
			if image, _ := snapshotterOf(t, sts); image != snapshotterImageV1 {
				t.Errorf("snapshotter = %q on a cluster serving v1, want %q", image, snapshotterImageV1)
			}
			return
		}
	}
	t.Fatal("no controller StatefulSet was built")
}

// Turning group snapshots off on a discovery error would restart the controller
// with a different sidecar on a blip, so the reconcile fails instead. That
// holds when the deployment asked for no volume snapshots too: the sidecar runs
// either way.
func TestAGroupSnapshotDiscoveryFailureFailsTheReconcile(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.EnableVolumeSnapshots = ptr.To(false)
	r := &SimplyblockDriverReconciler{Snapshots: fixedSnapshotAPI{served: true, groupErr: errors.New("timeout")}}

	if _, err := r.desired(t.Context(), d, testImage); err == nil {
		t.Error("the object set was built without knowing which group snapshot version is served")
	}
}
