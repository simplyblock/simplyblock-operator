// Tests for the NFSExport registry: create-or-fetch under a derived name, and
// the fact that it never touches status.
//
// Status is the operator's. The registry used to write the backing volume's
// identity there and raced the operator's own writes for it; the identity is
// now read off spec.volumeRef, so there is nothing to race.

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func fakeRegistryClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(nfsExportGVR.GroupVersion().WithKind("NFSExportList"),
		&unstructured.UnstructuredList{})
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{nfsExportGVR: "NFSExportList"}, objects...)
}

func existingExport(name, namespace string, status map[string]any) *unstructured.Unstructured {
	object := desiredExport(name, ExportSpec{
		Namespace: namespace, VolumeRef: "c:p:v", ExportPath: "/var/lib/simplyblock/exports/x",
	})
	if status != nil {
		_ = unstructured.SetNestedMap(object.Object, status, "status")
	}
	return object
}

// A record that already exists is read back rather than created again. The
// external provisioner retries CreateVolume, and the name is derived, so the
// second attempt must address the object the first one made.
func TestEnsureExportFetchesAnExistingRecord(t *testing.T) {
	const name = "nfsexp-test"
	client := fakeRegistryClient(existingExport(name, "team-a", map[string]any{"phase": "Ready"}))
	registry := &dynamicExportRegistry{client: client}

	record, err := registry.EnsureExport(context.Background(), name, ExportSpec{
		Namespace: "team-a", VolumeRef: "c:p:v", ExportPath: "/var/lib/simplyblock/exports/x",
	})
	if err != nil {
		t.Fatalf("EnsureExport: %v", err)
	}
	if record.Phase != "Ready" {
		t.Errorf("phase = %q, want Ready read back", record.Phase)
	}
}

// Recording an export writes the spec and nothing else.
//
// status.lvolID was the one field the CSI controller wrote onto a record the
// operator also writes, and the two raced on the same status object. Deriving
// the backing volume from spec.volumeRef instead removes the write, and with
// it the race and the retry loop that handled it. Asserted as "no status
// subresource write happens at all," because that is the property that makes
// the race impossible rather than merely unlikely.
func TestEnsureExportDoesNotWriteStatus(t *testing.T) {
	const name = "nfsexp-test"
	client := fakeRegistryClient()
	registry := &dynamicExportRegistry{client: client}

	if _, err := registry.EnsureExport(context.Background(), name, ExportSpec{
		Namespace: "team-a", VolumeRef: "c:p:v", ExportPath: "/var/lib/simplyblock/exports/x",
	}); err != nil {
		t.Fatalf("EnsureExport: %v", err)
	}

	for _, action := range client.Actions() {
		if action.GetSubresource() == "status" {
			t.Errorf("EnsureExport wrote the status subresource (%s), so it still races the operator",
				action.GetVerb())
		}
	}
}
