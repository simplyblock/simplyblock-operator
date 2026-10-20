package v1alpha2

import (
	"encoding/json"
	"strings"
	"testing"
)

// A group restore sets groupBackupID and no backupRef. The controller writes
// the whole object back (its finalizer update), and that write must not grow a
// backupRef: an empty struct serialized as {"name": ""} tripped the CEL rule
// ("set exactly one of backupRef and groupBackupID") and the name's minimum
// length, and the API server refused every reconcile (live 2026-10-20).
func TestAGroupRestoreSerializesNoBackupRef(t *testing.T) {
	ops := StorageBackupOps{Spec: StorageBackupOpsSpec{
		ClusterRef:    NamespacedDnsReference{Name: "cluster-a"},
		GroupBackupID: "e908ea4d-d025-42e2-aa87-526b6267a37d",
		Action:        StorageBackupOpsActionRestore,
	}}
	raw, err := json.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "backupRef") {
		t.Fatalf("an unset backupRef was serialized: %s", raw)
	}

	// A single-volume restore still carries its reference.
	ops.Spec.GroupBackupID = ""
	ops.Spec.BackupRef = NamespacedReference{Name: "a-backup"}
	raw, err = json.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"backupRef":{"name":"a-backup"}`) {
		t.Fatalf("a set backupRef was not serialized: %s", raw)
	}
}
