package groupsnapshot_test

import (
	"testing"

	volumegroupsnapshotv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumegroupsnapshot/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/simplyblock-operator/internal/groupsnapshot"
	"github.com/simplyblock/simplyblock-operator/internal/groupsnapshot/groupsnapshottest"
)

// v1Object is an object of a cluster that serves only groupsnapshot.storage.k8s.io/v1,
// which no Go type in the client library this module pins describes.
func v1Object(kind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	for k, v := range fields {
		u.Object[k] = v
	}
	u.SetAPIVersion(groupsnapshot.Group + "/v1")
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func v1Cluster(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithRESTMapper(groupsnapshottest.Mapper("v1")).
		WithObjects(objs...).
		Build()
}

// OpenShift 5 serves the kinds at v1 only. Every read the operator makes has
// to work there, which a typed v1beta1 read cannot: the API server has no
// v1beta1 resource to answer it.
func TestReadsAClusterThatServesOnlyV1(t *testing.T) {
	c := v1Cluster(t,
		v1Object(groupsnapshot.KindVolumeGroupSnapshot, "apps", "nightly", map[string]any{
			"spec": map[string]any{
				"volumeGroupSnapshotClassName": "simplyblock-group",
				"source": map[string]any{"selector": map[string]any{
					"matchLabels": map[string]any{"group": "db"},
				}},
			},
			"status": map[string]any{
				"readyToUse":                          true,
				"boundVolumeGroupSnapshotContentName": "content-1",
			},
		}),
		v1Object(groupsnapshot.KindVolumeGroupSnapshotClass, "", "simplyblock-group", map[string]any{
			"driver": "csi.simplyblock.io",
		}),
		v1Object(groupsnapshot.KindVolumeGroupSnapshotContent, "", "content-1", map[string]any{
			"spec": map[string]any{"source": map[string]any{
				"volumeHandles": []any{"a", "b", "c"},
			}},
		}),
	)

	vgs, err := groupsnapshot.GetVolumeGroupSnapshot(t.Context(), c, client.ObjectKey{Namespace: "apps", Name: "nightly"})
	if err != nil {
		t.Fatalf("read the VolumeGroupSnapshot: %v", err)
	}
	if got := ptr.Deref(vgs.Spec.VolumeGroupSnapshotClassName, ""); got != "simplyblock-group" {
		t.Errorf("class name = %q, want simplyblock-group", got)
	}
	if vgs.Spec.Source.Selector == nil || vgs.Spec.Source.Selector.MatchLabels["group"] != "db" {
		t.Errorf("selector = %+v, want group=db", vgs.Spec.Source.Selector)
	}
	if vgs.Status == nil || !ptr.Deref(vgs.Status.ReadyToUse, false) ||
		ptr.Deref(vgs.Status.BoundVolumeGroupSnapshotContentName, "") != "content-1" {
		t.Errorf("status = %+v, want ready and bound to content-1", vgs.Status)
	}

	class, err := groupsnapshot.GetVolumeGroupSnapshotClass(t.Context(), c, "simplyblock-group")
	if err != nil || class.Driver != "csi.simplyblock.io" {
		t.Errorf("class = %+v, %v; want driver csi.simplyblock.io", class, err)
	}

	content, err := groupsnapshot.GetVolumeGroupSnapshotContent(t.Context(), c, "content-1")
	if err != nil || len(content.Spec.Source.VolumeHandles) != 3 {
		t.Errorf("content = %+v, %v; want three volume handles", content, err)
	}
}

// A cluster that still serves only v1beta1 keeps working.
func TestReadsAClusterThatServesOnlyV1beta1(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := volumegroupsnapshotv1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(groupsnapshottest.Mapper("v1beta1")).
		WithObjects(&volumegroupsnapshotv1beta1.VolumeGroupSnapshotClass{
			ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-group"},
			Driver:     "csi.simplyblock.io",
		}).
		Build()

	class, err := groupsnapshot.GetVolumeGroupSnapshotClass(t.Context(), c, "simplyblock-group")
	if err != nil || class.Driver != "csi.simplyblock.io" {
		t.Errorf("class = %+v, %v; want driver csi.simplyblock.io", class, err)
	}
}

// A cluster serving several versions is read at the newest, which is the one
// whose shape the others are converted to.
func TestReadsTheNewestServedVersion(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithRESTMapper(groupsnapshottest.Mapper("v1beta1", "v1beta2", "v1")).
		WithObjects(v1Object(groupsnapshot.KindVolumeGroupSnapshotClass, "", "simplyblock-group",
			map[string]any{"driver": "csi.simplyblock.io"})).
		Build()

	if _, err := groupsnapshot.GetVolumeGroupSnapshotClass(t.Context(), c, "simplyblock-group"); err != nil {
		t.Errorf("the class stored at v1 was not read on a cluster serving v1: %v", err)
	}
}

// A cluster that serves no version of the group has no such object, and says
// so the way a missing object is said: callers already handle NotFound.
func TestAClusterWithoutTheKindsAnswersNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	_, err := groupsnapshot.GetVolumeGroupSnapshot(t.Context(), c, client.ObjectKey{Namespace: "apps", Name: "nightly"})
	if !apierrors.IsNotFound(err) {
		t.Errorf("error = %v, want NotFound", err)
	}
}

func TestAMissingObjectIsNotFound(t *testing.T) {
	c := v1Cluster(t)

	_, err := groupsnapshot.GetVolumeGroupSnapshotClass(t.Context(), c, "absent")
	if !apierrors.IsNotFound(err) {
		t.Errorf("error = %v, want NotFound", err)
	}
}

// Admission hands the object over as JSON at the version it was written in;
// the decoded shape is the same whichever version that is.
func TestDecodesAnAdmittedObjectOfEveryVersion(t *testing.T) {
	for _, version := range groupsnapshot.Versions {
		raw := []byte(`{"apiVersion":"` + groupsnapshot.Group + `/` + version + `","kind":"VolumeGroupSnapshot",` +
			`"metadata":{"name":"nightly","namespace":"apps"},` +
			`"spec":{"volumeGroupSnapshotClassName":"simplyblock-group","source":{"selector":{"matchLabels":{"group":"db"}}}}}`)
		vgs, err := groupsnapshot.DecodeVolumeGroupSnapshot(raw)
		if err != nil {
			t.Fatalf("%s: decode: %v", version, err)
		}
		if ptr.Deref(vgs.Spec.VolumeGroupSnapshotClassName, "") != "simplyblock-group" ||
			vgs.Spec.Source.Selector.MatchLabels["group"] != "db" {
			t.Errorf("%s: decoded %+v", version, vgs.Spec)
		}
	}
	if _, err := groupsnapshot.DecodeVolumeGroupSnapshot([]byte(`{"apiVersion":"` + groupsnapshot.Group +
		`/v2","kind":"VolumeGroupSnapshot"}`)); err == nil {
		t.Error("a version whose shape is unknown was decoded")
	}
}
