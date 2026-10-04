// Whether a pushed control-plane change wakes the node controller.
//
// These assert the source is built, not that an event arrives: a channel source
// only delivers under a running manager, and this package has no envtest suite
// to start one in. So the case below would not catch somebody deleting the
// WatchesRawSource call while leaving this helper -- what it catches is the
// wiring being dropped, which is how it was lost the first time.

package node

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

// aStreamingCache is a NodeCache with a trigger channel and nothing in it.
type aStreamingCache struct{ ch chan event.GenericEvent }

func (c *aStreamingCache) Triggers() <-chan event.GenericEvent { return c.ch }

func (c *aStreamingCache) Lookup(string) (cpinformer.Scope, subscriptions.NodeDTO, bool) {
	return cpinformer.Scope{}, subscriptions.NodeDTO{}, false
}
func (c *aStreamingCache) List(cpinformer.Scope) []subscriptions.NodeDTO { return nil }
func (c *aStreamingCache) Synced(cpinformer.Scope) bool                  { return true }

// TestAPushedNodeChangeWakesTheController is the wiring that was lost.
//
// Regression: 2026-09-21-the-node-stream-woke-nobody — the storage-node
// subscription kept publishing triggers and the controller stopped watching
// them when the kind moved to v1alpha2. Steady state went on reading node status
// out of the same subscription, so the informer looked connected and every
// status change waited out nodeRetry anyway. The stream was a cache, and it was
// built to be a push.
func TestAPushedNodeChangeWakesTheController(t *testing.T) {
	r := &StorageNodeReconciler{Nodes: &aStreamingCache{ch: make(chan event.GenericEvent, 1)}}

	if r.pushedNodeChanges() == nil {
		t.Error("the controller is not woken by the storage-node stream, so a status " +
			"change is noticed only when the requeue interval next comes round")
	}
}

// A deployment with no informer runs on its requeue interval and must not be
// given a source over a nil cache.
func TestNoInformerIsNoSource(t *testing.T) {
	r := &StorageNodeReconciler{}

	if r.pushedNodeChanges() != nil {
		t.Error("a deployment with no control-plane informer was given a stream source")
	}
}
