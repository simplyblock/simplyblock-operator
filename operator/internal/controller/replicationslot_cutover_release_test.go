package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// slotBackend is a fake control plane for the slot's backend reads: the
// relationship (nil = 404), the replication health, and per-volume reads.
type slotBackend struct {
	relationship *replVolumeReplicationStatus
	resyncing    bool
	// volumes maps a volume id to its do_replicate flag; a missing id is 404.
	volumes map[string]bool
	puts    int
}

func (b *slotBackend) serve(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication"):
		if b.relationship == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(b.relationship)
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication/status"):
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"role": "source", "state": "in_sync", "resyncing": b.resyncing,
		})
	case req.Method == http.MethodGet && strings.Contains(path, "/volumes/"):
		parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
		id := parts[len(parts)-1]
		doRepl, ok := b.volumes[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "do_replicate": doRepl})
	case req.Method == http.MethodPut:
		b.puts++
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func cutoverPendingSlot(observed bool) *simplyblockv1alpha1.ReplicationSlot {
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending))
	slot.Status.Message = "Cutover pending — awaiting replication_commit"
	slot.Status.TargetLvolID = "clone-id"
	slot.Status.TargetNQN = "nqn.clone"
	slot.Annotations = map[string]string{annotCutoverProceedSignaled: annotCutoverProceedSignaledValue}
	if observed {
		slot.Annotations[annotCutoverObserved] = annotCutoverObservedValue
	}
	return slot
}

// The final step failed / the budget ran out: the backend put the relationship
// back to replicating and cleared its target. The slot must leave
// cutover_pending (bf12 2026-11-04: stuck 16 h).
func TestSlot_CutoverPending_BackendBackToReplicating_Releases(t *testing.T) {
	b := &slotBackend{
		relationship: &replVolumeReplicationStatus{State: utils.ReplicationBackendStateReplicating, IsSource: true},
		volumes:      map[string]bool{"vol-id": true},
	}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSlotReconciler(t, cutoverPendingSlot(true), readyReplicationPolicy())

	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Fatalf("State = %q, want replicating", got.Status.State)
	}
	if got.Status.Direction != string(simplyblockv1alpha1.ReplicationSlotDirectionSource) {
		t.Errorf("Direction = %q, want source", got.Status.Direction)
	}
	if got.Status.TargetLvolID != "" || got.Status.TargetNQN != "" {
		t.Errorf("target not cleared: %q / %q", got.Status.TargetLvolID, got.Status.TargetNQN)
	}
	if _, ok := got.Annotations[annotCutoverObserved]; ok {
		t.Error("cutover-observed annotation must be cleared on release")
	}
	if _, ok := got.Annotations[annotCutoverProceedSignaled]; ok {
		t.Error("cutover-proceed-signaled must be cleared so the next attempt preconnects again")
	}

	// The next pass sees a replicating, still-replicating volume: steady state.
	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getSlot(t, cl); got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Errorf("State = %q after the next pass, want replicating", got.Status.State)
	}
}

// A relationship reverted to replicating that still names the deleted clone:
// the slot drops the target.
func TestSlot_CutoverPending_ReleasedTargetGone_ClearsTarget(t *testing.T) {
	b := &slotBackend{
		relationship: &replVolumeReplicationStatus{
			State: utils.ReplicationBackendStateReplicating, IsSource: true,
			TargetLvolID: "clone-id", TargetNQN: "nqn.clone",
			TargetClusterID: "tgt-cluster", TargetPoolID: "tgt-pool",
		},
		volumes: map[string]bool{"vol-id": true}, // clone-id: 404
	}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSlotReconciler(t, cutoverPendingSlot(true), readyReplicationPolicy())

	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Fatalf("State = %q, want replicating", got.Status.State)
	}
	if got.Status.TargetLvolID != "" || got.Status.TargetNQN != "" {
		t.Errorf("deleted target kept: %q / %q", got.Status.TargetLvolID, got.Status.TargetNQN)
	}
}

// A target that still exists is kept.
func TestSlot_CutoverPending_ReleasedTargetExists_KeepsTarget(t *testing.T) {
	b := &slotBackend{
		relationship: &replVolumeReplicationStatus{
			State: utils.ReplicationBackendStateReplicating, IsSource: true,
			TargetLvolID: "tgt-id", TargetNQN: "nqn.tgt",
			TargetClusterID: "tgt-cluster", TargetPoolID: "tgt-pool",
		},
		volumes: map[string]bool{"vol-id": true, "tgt-id": false},
	}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSlotReconciler(t, cutoverPendingSlot(true), readyReplicationPolicy())

	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getSlot(t, cl)
	if got.Status.TargetLvolID != "tgt-id" || got.Status.TargetNQN != "nqn.tgt" {
		t.Errorf("target = %q / %q, want tgt-id / nqn.tgt", got.Status.TargetLvolID, got.Status.TargetNQN)
	}
}

// Plan canceled, policy detached: no relationship, replication stopped. The
// slot leaves cutover_pending, then reflects the stopped replication by
// re-attaching its policy.
func TestSlot_CutoverPending_NoRelationshipReplicationStopped_ReleasesAndReattaches(t *testing.T) {
	b := &slotBackend{volumes: map[string]bool{"vol-id": false}}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSlotReconciler(t, cutoverPendingSlot(true), readyReplicationPolicy())

	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Fatalf("State = %q, want replicating", got.Status.State)
	}
	if !strings.Contains(got.Status.Message, "no active replication relationship") {
		t.Errorf("Message = %q", got.Status.Message)
	}

	// Next pass: the volume does not replicate -> not "replicating" any more.
	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got = getSlot(t, cl)
	if got.Status.State != "" {
		t.Fatalf("State = %q, want re-attach (empty) for a stopped replication", got.Status.State)
	}
	if got.Annotations[annotReattachedStopped] != annotReattachedStoppedValue {
		t.Error("re-attach marker not set")
	}

	// Next pass re-attaches the policy.
	b.volumes["vol-id"] = true
	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.puts != 1 {
		t.Errorf("PUT replication_policy_id calls = %d, want 1", b.puts)
	}
	if got := getSlot(t, cl); got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Errorf("State = %q after re-attach, want replicating", got.Status.State)
	}

	// Replicating again: the marker goes.
	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := getSlot(t, cl).Annotations[annotReattachedStopped]; ok {
		t.Error("re-attach marker must be cleared once the backend replicates again")
	}
}

// Commit queued, the task not yet at cutover_pending: a slot that never saw
// cutover_pending stays while the backend still runs the cutover task.
func TestSlot_CutoverPending_NotObservedTaskInFlight_Waits(t *testing.T) {
	b := &slotBackend{
		relationship: &replVolumeReplicationStatus{State: utils.ReplicationBackendStateReplicating, IsSource: true},
		resyncing:    true,
		volumes:      map[string]bool{"vol-id": true},
	}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSlotReconciler(t, cutoverPendingSlot(false), readyReplicationPolicy())

	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getSlot(t, cl); got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending) {
		t.Errorf("State = %q, want cutover_pending while the cutover task runs", got.Status.State)
	}
}

// ...and is released once that task ended without a cutover.
func TestSlot_CutoverPending_NotObservedTaskEnded_Releases(t *testing.T) {
	b := &slotBackend{volumes: map[string]bool{"vol-id": true}}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSlotReconciler(t, cutoverPendingSlot(false), readyReplicationPolicy())

	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getSlot(t, cl); got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Errorf("State = %q, want replicating once no cutover task runs", got.Status.State)
	}
}

// Seeing cutover_pending on the backend marks the attempt observed.
func TestSlot_CutoverPending_BackendPending_MarksObserved(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/replication"):
			_ = json.NewEncoder(w).Encode(replVolumeReplicationStatus{State: backendStateCutoverPending})
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/connect"):
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSlotReconciler(t, cutoverPendingSlot(false), readyReplicationPolicy())

	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getSlot(t, cl)
	if got.Annotations[annotCutoverObserved] != annotCutoverObservedValue {
		t.Error("cutover-observed annotation not set")
	}
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending) {
		t.Errorf("State = %q, want cutover_pending", got.Status.State)
	}
}

// A failback cutover polls the target cluster and is left to its own handling.
func TestSlot_CutoverPending_Failback_NotReleasedOnReplicating(t *testing.T) {
	b := &slotBackend{
		relationship: &replVolumeReplicationStatus{State: utils.ReplicationBackendStateReplicating},
		volumes:      map[string]bool{"vol-id": true},
	}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	slot := cutoverPendingSlot(true)
	slot.Annotations[annotFailbackTarget] = "tgt-cluster:tgt-pool:tgt-vol"
	r, cl := newSlotReconciler(t, slot, readyReplicationPolicy())

	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getSlot(t, cl); got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending) {
		t.Errorf("State = %q, want failback cutover_pending unchanged", got.Status.State)
	}
}

// A replicating slot whose policy the backend detached must not keep saying
// replicating; if a re-attach does not take, it goes to error instead of
// looping.
func TestSlot_Replicating_BackendStopped_ReattachThenError(t *testing.T) {
	b := &slotBackend{volumes: map[string]bool{"vol-id": false}}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateReplicating))
	slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionSource)
	r, cl := newSlotReconciler(t, slot, readyReplicationPolicy())

	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getSlot(t, cl); got.Status.State != "" {
		t.Fatalf("State = %q, want re-attach (empty)", got.Status.State)
	}
	// Re-attach (PUT) -> replicating.
	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.puts != 1 {
		t.Fatalf("PUT calls = %d, want 1", b.puts)
	}
	// Backend still reports do_replicate=false -> error, not another re-attach.
	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateError) {
		t.Errorf("State = %q, want error", got.Status.State)
	}
	if b.puts != 1 {
		t.Errorf("PUT calls = %d, want no second re-attach in the same pass", b.puts)
	}
}

// A target-side slot (the volume a fail-over or failback moved IO from) is not
// expected to replicate and is left alone.
func TestSlot_Replicating_TargetDirection_NotCheckedForStop(t *testing.T) {
	b := &slotBackend{volumes: map[string]bool{"vol-id": false}}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateReplicating))
	slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionTarget)
	r, cl := newSlotReconciler(t, slot, readyReplicationPolicy())

	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getSlot(t, cl); got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Errorf("State = %q, want replicating", got.Status.State)
	}
}
