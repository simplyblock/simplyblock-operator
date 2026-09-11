// Tests for the backup subscription: naming an object after a cluster the
// operator has adopted, and the seeding that stands in for a stream the control
// plane does not serve yet.

package subscriptions

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

const (
	backupCluster   = "11111111-1111-1111-1111-111111111111"
	backupID        = "44444444-4444-4444-4444-444444444444"
	backupClusterCR = "production"
	backupNamespace = "sb"
)

func backupScope() cpinformer.Scope { return cpinformer.Scope{backupCluster} }

func backupSnapshot(t *testing.T, backups ...BackupDTO) cpinformer.Event {
	t.Helper()
	data, err := json.Marshal(backups)
	if err != nil {
		t.Fatalf("encode the snapshot: %v", err)
	}
	return cpinformer.Event{Kind: cpinformer.EventSnapshot, Scope: backupScope(), Data: data}
}

func adoptedBackupSubscription() *BackupSubscription {
	s := NewBackupSubscription()
	s.RegisterCluster(backupCluster, types.NamespacedName{
		Namespace: backupNamespace, Name: backupClusterCR,
	})
	return s
}

func TestBackupSubscriptionNamesTheObjectAfterTheStoresIdentifier(t *testing.T) {
	s := adoptedBackupSubscription()

	if err := s.Ingest(context.Background(),
		backupSnapshot(t, BackupDTO{ID: backupID, Status: "completed"})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	key := types.NamespacedName{
		Namespace: backupNamespace,
		Name:      simplyblockv1alpha2.StorageBackupName(backupID),
	}
	scope, dto, ok := s.Lookup(key)
	if !ok {
		t.Fatalf("the cached backup is not reachable by its object key %s", key)
	}
	if dto.ID != backupID || scope.Key() != backupScope().Key() {
		t.Errorf("Lookup = %+v in %v, want the backup in its cluster's scope", dto, scope)
	}
}

// A backup reported for a cluster the operator has not adopted has nowhere to
// go: the object belongs in that cluster's namespace, and there is none.
// Caching it and naming nothing is the right pair, because the cluster's
// registration is followed by a snapshot that enqueues everything.
func TestBackupSubscriptionNamesNothingForAnUnadoptedCluster(t *testing.T) {
	s := NewBackupSubscription()

	if err := s.Ingest(context.Background(),
		backupSnapshot(t, BackupDTO{ID: backupID, Status: "completed"})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	key := types.NamespacedName{
		Namespace: backupNamespace,
		Name:      simplyblockv1alpha2.StorageBackupName(backupID),
	}
	if _, _, ok := s.Lookup(key); ok {
		t.Error("a backup of an unadopted cluster was named after a namespace nothing chose")
	}
	if _, _, cached := s.Find(backupID); !cached {
		t.Error("the backup was not cached, so the registration that follows would find nothing")
	}
}

// Seeding is what lets the export endpoint stand in for a stream the control
// plane does not serve yet. It has to leave the scope synced, or the mirror
// would treat every absence as a cold cache and never delete anything.
func TestSeedingMarksTheScopeSynced(t *testing.T) {
	s := adoptedBackupSubscription()

	if s.Synced(backupScope()) {
		t.Fatal("the scope is synced before anything was applied")
	}
	if err := s.Seed(context.Background(), backupScope(),
		[]BackupDTO{{ID: backupID, Status: "completed"}}); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if !s.Synced(backupScope()) {
		t.Error("the scope is not synced after a listing was applied")
	}
}

// A listing replaces its scope rather than merging into it, so a backup that
// left the store between two listings is forgotten.
func TestSeedingForgetsABackupTheListingOmits(t *testing.T) {
	s := adoptedBackupSubscription()
	const gone = "55555555-5555-5555-5555-555555555555"

	if err := s.Seed(context.Background(), backupScope(), []BackupDTO{
		{ID: backupID, Status: "completed"},
		{ID: gone, Status: "completed"},
	}); err != nil {
		t.Fatalf("the first listing: %v", err)
	}
	if err := s.Seed(context.Background(), backupScope(),
		[]BackupDTO{{ID: backupID, Status: "completed"}}); err != nil {
		t.Fatalf("the second listing: %v", err)
	}

	if _, _, cached := s.Find(gone); cached {
		t.Error("a backup the second listing omitted is still cached")
	}
	if _, _, cached := s.Find(backupID); !cached {
		t.Error("a backup both listings reported was forgotten")
	}
}

func TestBackupSubscriptionStreamsPerCluster(t *testing.T) {
	s := NewBackupSubscription()
	if got, want := s.Path(backupScope()), "/api/v2/clusters/"+backupCluster+"/backups/"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestBackupPolicySubscriptionNamesTheObjectAfterThePolicysName(t *testing.T) {
	s := NewBackupPolicySubscription()
	s.RegisterCluster(backupCluster, types.NamespacedName{
		Namespace: backupNamespace, Name: backupClusterCR,
	})

	if err := s.Seed(context.Background(), backupScope(), []BackupPolicyDTO{
		{ID: "66666666-6666-6666-6666-666666666666", Name: "nightly", Status: "active"},
	}); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	key := types.NamespacedName{Namespace: backupNamespace, Name: "nightly"}
	_, dto, ok := s.Lookup(key)
	if !ok {
		t.Fatalf("the cached policy is not reachable by its object key %s", key)
	}
	if dto.Name != "nightly" {
		t.Errorf("Lookup = %+v, want the nightly policy", dto)
	}
}
