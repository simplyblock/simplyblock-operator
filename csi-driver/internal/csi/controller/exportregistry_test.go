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

// A client mounts the export's Service, the address that survives the export
// moving to a different host, not the bound host's own address
// (design-pnfs-rwx.md §13.3). A regression back to reading mdsNodeIP would
// pass silently here otherwise, since both are plain strings.
func TestEnsureExportReadsTheServiceAddressNotTheHostAddress(t *testing.T) {
	const name = "nfsexp-test"
	client := fakeRegistryClient(existingExport(name, "team-a", map[string]any{
		"phase":          "Ready",
		"mdsNodeIP":      "192.168.10.9",
		"serviceAddress": "10.96.5.5",
	}))
	registry := &dynamicExportRegistry{client: client}

	record, err := registry.EnsureExport(context.Background(), name, ExportSpec{
		Namespace: "team-a", VolumeRef: "c:p:v", ExportPath: "/var/lib/simplyblock/exports/x",
	})
	if err != nil {
		t.Fatalf("EnsureExport: %v", err)
	}
	if record.ServiceAddress != "10.96.5.5" {
		t.Errorf("ServiceAddress = %q, want the Service's ClusterIP, not the host's address", record.ServiceAddress)
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

// Regression: 2026-09-23-pnfs-encryption-dropped -- encrypted RWX volumes lost
// their encryption bit when CSI created the export record, so the MDS treated
// ciphertext as an existing plaintext filesystem.
func TestDesiredExportRecordsEncryption(t *testing.T) {
	object := desiredExport("nfsexp-test", ExportSpec{
		Namespace: "team-a", VolumeRef: "c:p:v", ExportPath: "/exports/x", Encrypted: true,
	})

	got, found, err := unstructured.NestedBool(object.Object, "spec", "encrypted")
	if err != nil {
		t.Fatalf("reading spec.encrypted: %v", err)
	}
	if !found || !got {
		t.Fatalf("spec.encrypted = %t (found %t), want true", got, found)
	}
}
