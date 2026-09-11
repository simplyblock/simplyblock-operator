// The backup subscription: it streams one cluster's backups, decodes and caches
// them, and enqueues a reconcile trigger naming the StorageBackup object each
// one mirrors. It lives here rather than in the controller package because
// retrieval, decoding, and caching are the subscription's concerns; writing
// Kubernetes objects is the reconciler's.
//
// It carries one method the device subscription does not, Seed, because this
// layer has no fallback. A StorageBackup exists only because the stream reported
// the backup (design-storagebackup.md §5.1), so a stream the control plane does
// not yet serve is not a stale inventory but no inventory at all. Seed applies a
// listing as though it were the snapshot frame a stream opens with, which is what
// lets the export endpoint stand in until the subscription lands
// (design-storagebackup.md §10).

package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

// BackupDTO is the operator's view of a control-plane backup, matching the v2
// backup wire schema. Unknown fields are ignored on decode, so a field added
// upstream does not break it.
//
// What is absent is as telling as what is here: the control plane reports the
// logical volume a backup was taken from and not the claim that volume backs, so
// the mirror resolves the claim, the pool, and the filesystem from the
// PersistentVolume carrying that volume's handle. Which of status.source the
// store itself could answer is design-storagebackup.md §14 Q3, and until that is
// settled the Kubernetes side is where those facts come from.
type BackupDTO struct {
	ID string `json:"id"`
	// SourceClusterID is the cluster that wrote the copy, which is not
	// necessarily the cluster whose stream reported it: a store several clusters
	// have configured reports every backup in it to all of them. The reporting
	// cluster is the stream's scope rather than a field.
	SourceClusterID string `json:"source_cluster_id"`
	S3ID            int64  `json:"s3_id"`
	LvolID          string `json:"lvol_id"`
	LvolName        string `json:"lvol_name"`
	SnapshotID      string `json:"snapshot_id"`
	SnapshotName    string `json:"snapshot_name"`
	NodeID          string `json:"node_id"`
	Status          string `json:"status"`
	PrevBackupID    string `json:"prev_backup_id"`
	Size            int64  `json:"size"`
	CreatedAt       int64  `json:"created_at"`
	CompletedAt     int64  `json:"completed_at"`
}

// BackupSubscription streams a cluster's backups (one stream per cluster),
// decodes them into an in-memory cache, and enqueues a reconcile trigger naming
// the affected StorageBackup object. It performs no Kubernetes writes; a
// reconciler consumes its cache and trigger channel.
//
// A backup object is created beside the StorageCluster whose store holds it, and
// the control plane knows nothing of Kubernetes namespaces, so the subscription
// keeps the backend-cluster-id-to-object mapping that the StorageCluster
// controller registers. That is what lets Ingest name an object without reading
// the API — it runs on the stream goroutine and must never block on I/O.
type BackupSubscription struct {
	*Cache[BackupDTO]
	ClusterRegistry

	ch chan event.GenericEvent

	// byObject indexes the cache the way the reconciler reads it: a reconcile
	// request carries an object key, and only the subscription can turn one back
	// into a backup id, because only it knows the naming rule and the cluster
	// objects it depends on. It holds an entry for every cached backup and
	// nothing else, so a miss means the control plane no longer reports it.
	indexMu  sync.Mutex
	byObject map[string]string // "namespace/name" -> backend backup id
}

// NewBackupSubscription returns a backup subscription. It is not told a
// namespace: each backup object belongs beside the StorageCluster whose store
// holds it, which RegisterCluster supplies.
func NewBackupSubscription() *BackupSubscription {
	return &BackupSubscription{
		Cache:           NewCache(func(b BackupDTO) string { return b.ID }),
		ClusterRegistry: newClusterRegistry(),
		ch:              make(chan event.GenericEvent, 1024),
		byObject:        map[string]string{},
	}
}

// Name implements cpinformer.Subscription.
func (s *BackupSubscription) Name() string { return "backup" }

// Path implements cpinformer.Subscription: backups are scoped per cluster, so
// one stream is opened per StorageCluster.
func (s *BackupSubscription) Path(scope cpinformer.Scope) string {
	return fmt.Sprintf("/api/v2/clusters/%s/backups/", scope[0])
}

// Ingest implements cpinformer.Subscription: it decodes the event into the
// cache, then enqueues a reconcile trigger for each affected backup's
// StorageBackup object. It performs no API I/O, so it never stalls the stream
// loop.
func (s *BackupSubscription) Ingest(ctx context.Context, ev cpinformer.Event) error {
	return s.Cache.Ingest(ev, func(scope cpinformer.Scope, backupID string, present bool) {
		// The index is added before the trigger and dropped after it, so a
		// reconcile can always resolve the object it was handed back to a
		// backup id — including the reconcile that learns the backup is gone.
		if present {
			s.index(scope, backupID)
			s.enqueue(ctx, scope, backupID)
			return
		}
		s.enqueue(ctx, scope, backupID)
		s.unindex(scope, backupID)
	})
}

// Seed applies a listing to one scope as though it were the stream's opening
// snapshot: present backups are cached and triggered, and anything the listing
// omits is forgotten and triggered so its object is reconciled away. After it
// runs the scope counts as synced, which is what lets the reconciler act on an
// absence rather than treating every miss as a cold cache.
//
// It exists for the window in which the control plane serves the export
// endpoint and not the stream. A caller that has both should not use it: a
// reconnecting stream carries its own snapshot, so seeding beside one is a
// second writer of the same cache with an older view.
func (s *BackupSubscription) Seed(ctx context.Context, scope cpinformer.Scope, backups []BackupDTO) error {
	data, err := json.Marshal(backups)
	if err != nil {
		return fmt.Errorf("encode the backup listing as a snapshot: %w", err)
	}
	return s.Ingest(ctx, cpinformer.Event{Kind: cpinformer.EventSnapshot, Scope: scope, Data: data})
}

// index and unindex keep byObject in step with the cache. Both are no-ops while
// the backup's cluster has no registered name, which is the same condition under
// which no trigger is emitted: without a namespace there is no object key, so
// there is nothing to index and nothing to reconcile.
func (s *BackupSubscription) index(scope cpinformer.Scope, backupID string) {
	if key, ok := s.objectKey(scope, backupID); ok {
		s.indexMu.Lock()
		s.byObject[key.String()] = backupID
		s.indexMu.Unlock()
	}
}

func (s *BackupSubscription) unindex(scope cpinformer.Scope, backupID string) {
	if key, ok := s.objectKey(scope, backupID); ok {
		s.indexMu.Lock()
		delete(s.byObject, key.String())
		s.indexMu.Unlock()
	}
}

func (s *BackupSubscription) objectKey(scope cpinformer.Scope, backupID string) (types.NamespacedName, bool) {
	cluster, ok := s.cluster(scope[0])
	if !ok {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{
		Namespace: cluster.Namespace,
		Name:      simplyblockv1alpha2.StorageBackupName(backupID),
	}, true
}

// enqueue pushes a reconcile trigger naming the backup's StorageBackup object,
// giving up only on shutdown rather than dropping it when the channel is full
// (see cpinformer.Subscription on why waiting is the right side to err on).
//
// A backup whose cluster has no registered object name yields no trigger: the
// object belongs in that cluster's namespace, and there is nowhere to put it
// yet. The cluster's registration is followed by the stream's snapshot, which
// enqueues everything.
func (s *BackupSubscription) enqueue(ctx context.Context, scope cpinformer.Scope, backupID string) {
	key, ok := s.objectKey(scope, backupID)
	if !ok {
		return
	}
	sb := &simplyblockv1alpha2.StorageBackup{}
	sb.SetNamespace(key.Namespace)
	sb.SetName(key.Name)
	select {
	case s.ch <- event.GenericEvent{Object: sb}:
	case <-ctx.Done():
	}
}

// Triggers is the reconcile-trigger channel; the reconciler attaches it via
// source.Channel. Each event names the StorageBackup object to reconcile.
func (s *BackupSubscription) Triggers() <-chan event.GenericEvent { return s.ch }

// Lookup returns the cached backup that the named StorageBackup object mirrors,
// with the scope it belongs to, or ok=false if the control plane no longer
// reports it. It takes an object key rather than a backup id because that is
// what a reconcile request carries, and the namespace is part of the identity:
// two namespaces may each hold a cluster with the same store configured.
func (s *BackupSubscription) Lookup(key types.NamespacedName) (cpinformer.Scope, BackupDTO, bool) {
	s.indexMu.Lock()
	backupID, ok := s.byObject[key.String()]
	s.indexMu.Unlock()
	if !ok {
		return nil, BackupDTO{}, false
	}
	return s.Find(backupID)
}

var _ cpinformer.Subscription = (*BackupSubscription)(nil)
