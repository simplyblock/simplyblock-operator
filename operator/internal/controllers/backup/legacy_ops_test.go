// LegacyBackupOps finds the StorageBackupOps a typed decode cannot read, which
// is what lets the operator skip the ops controller instead of crash-looping.

package backup

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// opsReader answers a list of StorageBackupOps with fixed objects, and fails it
// when err is set. A typed client would fail to decode the legacy shape, which
// is why the code under test reads unstructured.
type opsReader struct {
	client.Reader
	items []map[string]any
	err   error
}

func (r opsReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	if r.err != nil {
		return r.err
	}
	u := list.(*unstructured.UnstructuredList)
	for _, item := range r.items {
		u.Items = append(u.Items, unstructured.Unstructured{Object: item})
	}
	return nil
}

func ops(namespace, name string, spec map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "storage.simplyblock.io/v1alpha2",
		"kind":       "StorageBackupOps",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"spec":       spec,
	}
}

func TestLegacyBackupOpsFindsStringReferences(t *testing.T) {
	object := map[string]any{"name": "nightly"}
	reader := opsReader{items: []map[string]any{
		ops("app", "current", map[string]any{"backupRef": object, "clusterRef": object}),
		ops("app", "both-strings", map[string]any{"backupRef": "nightly", "clusterRef": "c1"}),
		ops("team", "backup-only", map[string]any{"backupRef": "nightly", "clusterRef": object}),
		ops("team", "cluster-only", map[string]any{"backupRef": object, "clusterRef": "c1"}),
	}}

	got, err := LegacyBackupOps(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	want := []types.NamespacedName{
		{Namespace: "app", Name: "both-strings"},
		{Namespace: "team", Name: "backup-only"},
		{Namespace: "team", Name: "cluster-only"},
	}
	if len(got) != len(want) {
		t.Fatalf("legacy ops = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("legacy ops = %v, want %v", got, want)
		}
	}
}

func TestLegacyBackupOpsFindsNothingWhenAllCurrent(t *testing.T) {
	object := map[string]any{"name": "nightly"}
	reader := opsReader{items: []map[string]any{
		ops("app", "current", map[string]any{"backupRef": object, "clusterRef": object}),
	}}

	got, err := LegacyBackupOps(context.Background(), reader)
	if err != nil || len(got) != 0 {
		t.Fatalf("legacy ops = %v, %v; want none", got, err)
	}
}

func TestLegacyBackupOpsReturnsTheListError(t *testing.T) {
	boom := errors.New("forbidden")
	if _, err := LegacyBackupOps(context.Background(), opsReader{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
