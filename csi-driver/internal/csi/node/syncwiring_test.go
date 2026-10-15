package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/simplyblock/atlas/kube"
)

func TestReadNodeSite(t *testing.T) {
	// labeledNode carries zone-a, and topologyServer names the driver after it.
	ns := topologyServer(t, labeledNode(nil))
	site, err := ns.readNodeSite(context.Background())
	if err != nil || site != "zone-a" {
		t.Fatalf("site=%q err=%v, want zone-a/nil", site, err)
	}

	// A node with no zone label cannot name its site: error, never a guess.
	bare := topologyServer(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	if _, err := bare.readNodeSite(context.Background()); err == nil {
		t.Error("a node with no zone label must error, not resolve a site")
	}
}

func TestPatchVolumeReplicationSite(t *testing.T) {
	vr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "replication.storage.openshift.io/v1alpha1",
		"kind":       "VolumeReplication",
		"metadata":   map[string]interface{}{"name": "vr1", "namespace": "app"},
		"spec": map[string]interface{}{
			"volumeReplicationClass": "sb-sync",
			"replicationState":       "secondary",
			"dataSource":             map[string]interface{}{"kind": "PersistentVolumeClaim", "name": "claim1"},
		},
	}}
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{kube.VolumeReplicationGVR: "VolumeReplicationList"}, vr)

	err := patchVolumeReplicationSite(context.Background(), dc,
		kube.VolumeReplicationRef{Namespace: "app", Name: "vr1"}, testSiteA, "primary")
	if err != nil {
		t.Fatalf("patch: %v", err)
	}

	got, err := dc.Resource(kube.VolumeReplicationGVR).Namespace("app").
		Get(context.Background(), "vr1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	site := got.GetAnnotations()[kube.SyncSiteAnnotation]
	state, _, _ := unstructured.NestedString(got.Object, "spec", "replicationState")
	if site != testSiteA || state != "primary" {
		t.Errorf("after patch: site=%q state=%q, want site-a/primary", site, state)
	}

	// The same helper sets secondary when the switch demotes first (§6.4).
	if err := patchVolumeReplicationSite(context.Background(), dc,
		kube.VolumeReplicationRef{Namespace: "app", Name: "vr1"}, testSiteA, "secondary"); err != nil {
		t.Fatalf("patch secondary: %v", err)
	}
	got, _ = dc.Resource(kube.VolumeReplicationGVR).Namespace("app").
		Get(context.Background(), "vr1", metav1.GetOptions{})
	if state, _, _ := unstructured.NestedString(got.Object, "spec", "replicationState"); state != "secondary" {
		t.Errorf("after secondary patch: state=%q, want secondary", state)
	}
	// The merge leaves the immutable class and the admin's dataSource untouched.
	class, _, _ := unstructured.NestedString(got.Object, "spec", "volumeReplicationClass")
	if class != "sb-sync" {
		t.Errorf("class was disturbed: %q, want sb-sync", class)
	}
	ds, _, _ := unstructured.NestedString(got.Object, "spec", "dataSource", "name")
	if ds != "claim1" {
		t.Errorf("dataSource was disturbed: %q, want claim1", ds)
	}
}
