// The schema's own rules for the two namespaced references of StorageBackupOps,
// run against a real apiserver.
//
// The objects are built unstructured, so the test states the wire shape of
// spec.clusterRef and spec.restore.claim and does not depend on the Go type that
// generates it.

package backup

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// restoreObject is a well-formed restore, which every case below starts from.
func restoreObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.simplyblock.io/v1alpha2",
		"kind":       "StorageBackupOps",
		"metadata":   map[string]any{"generateName": "cel-", "namespace": "default"},
		"spec": map[string]any{
			"clusterRef": map[string]any{"name": "production", "namespace": "infra"},
			"backupRef":  "backup-1",
			"action":     "Restore",
			"restore": map[string]any{
				"claim":      map[string]any{"name": "restored", "namespace": "team-b"},
				"targetPool": "pool-a",
			},
		},
	}}
}

func create(t *testing.T, obj *unstructured.Unstructured) error {
	t.Helper()
	ctx := context.Background()
	apiClient := apiServer(t)
	err := apiClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = apiClient.Delete(ctx, obj) })
	}
	return err
}

// The namespace of a reference is optional, and omitting it keeps the object
// free of a value the operation's namespace would fill in by reading.
func TestStorageBackupOpsAcceptsReferencesWithoutANamespace(t *testing.T) {
	obj := restoreObject()
	unstructured.RemoveNestedField(obj.Object, "spec", "clusterRef", "namespace")
	unstructured.RemoveNestedField(obj.Object, "spec", "restore", "claim", "namespace")

	if err := create(t, obj); err != nil {
		t.Fatalf("references naming no namespace were rejected: %v", err)
	}
	if _, found, _ := unstructured.NestedString(obj.Object, "spec", "clusterRef", "namespace"); found {
		t.Error("the apiserver stored a namespace the user did not write")
	}
}

func TestStorageBackupOpsRequiresTheNameOfEachReference(t *testing.T) {
	for _, path := range [][]string{{"spec", "clusterRef"}, {"spec", "restore", "claim"}} {
		t.Run(strings.Join(path, "."), func(t *testing.T) {
			obj := restoreObject()
			unstructured.RemoveNestedField(obj.Object, append(path, "name")...)

			err := create(t, obj)
			if err == nil {
				t.Fatal("a reference with no name was accepted")
			}
			if !strings.Contains(err.Error(), "name") {
				t.Errorf("rejected for the wrong reason: %v", err)
			}
		})
	}
}

// A StorageCluster name is at most 63 characters, and the reference's name
// field is shared with a claim, whose name may be longer.
func TestStorageBackupOpsBoundsTheClusterNameAndNotTheClaimName(t *testing.T) {
	long := strings.Repeat("a", 64)

	cluster := restoreObject()
	_ = unstructured.SetNestedField(cluster.Object, long, "spec", "clusterRef", "name")
	if err := create(t, cluster); err == nil || !strings.Contains(err.Error(), "at most 63") {
		t.Errorf("a 64-character cluster name was not refused for its length: %v", err)
	}

	claim := restoreObject()
	_ = unstructured.SetNestedField(claim.Object, long, "spec", "restore", "claim", "name")
	if err := create(t, claim); err != nil {
		t.Errorf("a 64-character claim name was refused: %v", err)
	}
}

// A string where the object now is names the break: an object written before
// the change no longer validates.
func TestStorageBackupOpsRejectsAStringClusterRef(t *testing.T) {
	obj := restoreObject()
	_ = unstructured.SetNestedField(obj.Object, "production", "spec", "clusterRef")

	if err := create(t, obj); err == nil {
		t.Fatal("a string spec.clusterRef was accepted")
	}
}

// Each reference is frozen as a whole: name and namespace cannot be edited, set
// when they were omitted, or removed.
func TestStorageBackupOpsFreezesBothReferences(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*unstructured.Unstructured)
	}{
		{"clusterRef name", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "other", "spec", "clusterRef", "name")
		}},
		{"clusterRef namespace", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "elsewhere", "spec", "clusterRef", "namespace")
		}},
		{"clusterRef namespace removed", func(o *unstructured.Unstructured) {
			unstructured.RemoveNestedField(o.Object, "spec", "clusterRef", "namespace")
		}},
		{"claim name", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "other", "spec", "restore", "claim", "name")
		}},
		{"claim namespace", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "elsewhere", "spec", "restore", "claim", "namespace")
		}},
		{"claim namespace removed", func(o *unstructured.Unstructured) {
			unstructured.RemoveNestedField(o.Object, "spec", "restore", "claim", "namespace")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := restoreObject()
			if err := create(t, obj); err != nil {
				t.Fatalf("creating the operation: %v", err)
			}
			tc.edit(obj)

			err := apiServer(t).Update(context.Background(), obj)
			if err == nil {
				t.Fatal("the apiserver accepted an edit to a frozen reference")
			}
			if !strings.Contains(err.Error(), "immutable") {
				t.Errorf("rejected for the wrong reason: %v", err)
			}
		})
	}
}

// A namespace omitted at creation stays omitted: the reference cannot be edited
// into naming one afterward.
func TestStorageBackupOpsFreezesAnOmittedNamespace(t *testing.T) {
	obj := restoreObject()
	unstructured.RemoveNestedField(obj.Object, "spec", "clusterRef", "namespace")
	if err := create(t, obj); err != nil {
		t.Fatalf("creating the operation: %v", err)
	}
	_ = unstructured.SetNestedField(obj.Object, "infra", "spec", "clusterRef", "namespace")

	err := apiServer(t).Update(context.Background(), obj)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("a namespace was added to a reference that omitted it: %v", err)
	}
}

var _ client.Object = &unstructured.Unstructured{}
