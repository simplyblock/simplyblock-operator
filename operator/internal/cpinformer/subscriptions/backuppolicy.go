// The backup-policy subscription: it streams one cluster's backup policies and
// caches them, so that the policy reconciler reads what the control plane did
// rather than asking it.
//
// It is the read half of a loop whose write half stays on the HTTP API. A policy
// is desired state the operator converges, so creating, updating, attaching, and
// detaching are requests with responses, and only the outcome is streamed
// (design-storagebackup.md §10). That is the difference from the backup stream
// next door, where the operator asks for nothing and the stream is the whole
// source of truth.

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

// BackupPolicyDTO is the operator's view of a control-plane backup policy,
// matching the v2 wire schema. Unknown fields are ignored on decode.
//
// Name is what ties one back to a Kubernetes object: the operator names a
// policy in the control plane after the StorageBackupPolicy that asked for it,
// so the name is the mapping and no second registry is needed for policies the
// way one is for clusters.
type BackupPolicyDTO struct {
	ID             string `json:"id"`
	ClusterID      string `json:"cluster_id"`
	Name           string `json:"name"`
	MaxVersions    int32  `json:"max_versions"`
	MaxAge         string `json:"max_age"`
	BackupSchedule string `json:"backup_schedule"`
	Status         string `json:"status"`
	LastBackupAt   int64  `json:"last_backup_at"`
}

// BackupPolicySubscription streams a cluster's backup policies (one stream per
// cluster), decodes them into an in-memory cache, and enqueues a reconcile
// trigger naming the StorageBackupPolicy object each one belongs to.
type BackupPolicySubscription struct {
	*Cache[BackupPolicyDTO]
	ClusterRegistry

	ch chan event.GenericEvent

	indexMu  sync.Mutex
	byObject map[string]string // "namespace/name" -> backend policy id
}

// NewBackupPolicySubscription returns a backup-policy subscription.
func NewBackupPolicySubscription() *BackupPolicySubscription {
	return &BackupPolicySubscription{
		Cache:           NewCache(func(p BackupPolicyDTO) string { return p.ID }),
		ClusterRegistry: newClusterRegistry(),
		ch:              make(chan event.GenericEvent, 1024),
		byObject:        map[string]string{},
	}
}

// Name implements cpinformer.Subscription.
func (s *BackupPolicySubscription) Name() string { return "backup-policy" }

// Path implements cpinformer.Subscription: policies are scoped per cluster.
func (s *BackupPolicySubscription) Path(scope cpinformer.Scope) string {
	return fmt.Sprintf("/api/v2/clusters/%s/backups/backup-policies/", scope[0])
}

// Ingest implements cpinformer.Subscription. It performs no API I/O, so it
// never stalls the stream loop.
func (s *BackupPolicySubscription) Ingest(ctx context.Context, ev cpinformer.Event) error {
	return s.Cache.Ingest(ev, func(scope cpinformer.Scope, policyID string, present bool) {
		if present {
			s.index(scope, policyID)
			s.enqueue(ctx, scope, policyID)
			return
		}
		s.enqueue(ctx, scope, policyID)
		s.unindex(scope, policyID)
	})
}

// Seed applies a listing to one scope as though it were the stream's opening
// snapshot. It stands in for the subscription while the control plane serves
// the list endpoint and not the stream, and it must not be used beside a live
// stream, which carries its own snapshot on every reconnect.
func (s *BackupPolicySubscription) Seed(
	ctx context.Context, scope cpinformer.Scope, policies []BackupPolicyDTO,
) error {
	data, err := json.Marshal(policies)
	if err != nil {
		return fmt.Errorf("encode the backup-policy listing as a snapshot: %w", err)
	}
	return s.Ingest(ctx, cpinformer.Event{Kind: cpinformer.EventSnapshot, Scope: scope, Data: data})
}

func (s *BackupPolicySubscription) index(scope cpinformer.Scope, policyID string) {
	if key, ok := s.objectKey(scope, policyID); ok {
		s.indexMu.Lock()
		s.byObject[key.String()] = policyID
		s.indexMu.Unlock()
	}
}

func (s *BackupPolicySubscription) unindex(scope cpinformer.Scope, policyID string) {
	if key, ok := s.objectKey(scope, policyID); ok {
		s.indexMu.Lock()
		delete(s.byObject, key.String())
		s.indexMu.Unlock()
	}
}

// objectKey names the StorageBackupPolicy a control-plane policy belongs to.
// The policy's own name is the object's name, because that is the name the
// operator created it under; a policy somebody created outside the operator has
// no object and yields nothing rather than a guess.
func (s *BackupPolicySubscription) objectKey(
	scope cpinformer.Scope, policyID string,
) (types.NamespacedName, bool) {
	cluster, ok := s.cluster(scope[0])
	if !ok {
		return types.NamespacedName{}, false
	}
	_, dto, cached := s.Find(policyID)
	if !cached || dto.Name == "" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: cluster.Namespace, Name: dto.Name}, true
}

func (s *BackupPolicySubscription) enqueue(
	ctx context.Context, scope cpinformer.Scope, policyID string,
) {
	key, ok := s.objectKey(scope, policyID)
	if !ok {
		return
	}
	policy := &simplyblockv1alpha2.StorageBackupPolicy{}
	policy.SetNamespace(key.Namespace)
	policy.SetName(key.Name)
	select {
	case s.ch <- event.GenericEvent{Object: policy}:
	case <-ctx.Done():
	}
}

// Triggers is the reconcile-trigger channel; the reconciler attaches it via
// source.Channel.
func (s *BackupPolicySubscription) Triggers() <-chan event.GenericEvent { return s.ch }

// Lookup returns the cached policy the named StorageBackupPolicy object asked
// for, or ok=false when the control plane no longer reports one.
func (s *BackupPolicySubscription) Lookup(
	key types.NamespacedName,
) (cpinformer.Scope, BackupPolicyDTO, bool) {
	s.indexMu.Lock()
	policyID, ok := s.byObject[key.String()]
	s.indexMu.Unlock()
	if !ok {
		return nil, BackupPolicyDTO{}, false
	}
	return s.Find(policyID)
}

var _ cpinformer.Subscription = (*BackupPolicySubscription)(nil)
