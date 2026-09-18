// Tests for the NFSExport registry, and specifically for the one thing that
// cannot be left to a retry: the backing volume's identity on the record.
//
// The CSI controller is the only component that knows which logical volume an
// export is for, because it just created it. The operator cannot assemble
// without it and holds, so an identity that is written once and never checked
// again is a deadlock rather than a delay.

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		Namespace: namespace, VolumeRef: "nfs:c:p:v", ExportPath: "/mnt/x", FSID: "f",
	})
	if status != nil {
		_ = unstructured.SetNestedMap(object.Object, status, "status")
	}
	return object
}

// A record that exists without the backing volume's identity gets it written.
//
// The create path wrote it and the fetch paths did not, so a status write that
// lost a race with the operator's own was never retried: every later
// CreateVolume took the fetch path, returned Aborted because the export was not
// Ready, and the operator held forever on an identity that never arrived.
func TestEnsureExportWritesTheIdentityOntoAnExistingRecord(t *testing.T) {
	const name = "nfsexp-test"
	client := fakeRegistryClient(existingExport(name, "team-a", map[string]any{"phase": "Assembling"}))
	registry := &dynamicExportRegistry{client: client}

	if _, err := registry.EnsureExport(context.Background(), name, ExportSpec{
		Namespace: "team-a", VolumeRef: "nfs:c:p:v", ExportPath: "/mnt/x", FSID: "f",
		LVolID: "0dacf0c3-9abc-4fda-b69a-a5285bf01cee",
	}); err != nil {
		t.Fatalf("EnsureExport: %v", err)
	}

	got, err := client.Resource(nfsExportGVR).Namespace("team-a").
		Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the record back: %v", err)
	}
	lvolID, _, _ := unstructured.NestedString(got.Object, "status", "lvolID")
	if lvolID != "0dacf0c3-9abc-4fda-b69a-a5285bf01cee" {
		t.Errorf("status.lvolID = %q, want the backing volume; the operator cannot assemble without it", lvolID)
	}
	phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
	if phase != "Assembling" {
		t.Errorf("phase = %q, want Assembling kept; the identity write must not reset the operator's phase", phase)
	}
}

// An identity already on the record is left alone, so the write is idempotent
// and does not churn the object on every retried CreateVolume.
func TestEnsureExportLeavesAnIdentityItAlreadyHas(t *testing.T) {
	const name = "nfsexp-test"
	client := fakeRegistryClient(existingExport(name, "team-a", map[string]any{
		"phase": "Ready", "lvolID": "already-there", "nguid": "n",
	}))
	registry := &dynamicExportRegistry{client: client}

	record, err := registry.EnsureExport(context.Background(), name, ExportSpec{
		Namespace: "team-a", VolumeRef: "nfs:c:p:v", ExportPath: "/mnt/x", FSID: "f",
		LVolID: "different",
	})
	if err != nil {
		t.Fatalf("EnsureExport: %v", err)
	}
	if record.Phase != "Ready" {
		t.Errorf("phase = %q, want Ready read back", record.Phase)
	}
	got, _ := client.Resource(nfsExportGVR).Namespace("team-a").
		Get(context.Background(), name, metav1.GetOptions{})
	lvolID, _, _ := unstructured.NestedString(got.Object, "status", "lvolID")
	if lvolID != "already-there" {
		t.Errorf("status.lvolID = %q, want the original kept", lvolID)
	}
}
