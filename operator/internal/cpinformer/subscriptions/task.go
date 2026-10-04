// The task subscription: it streams one cluster's asynchronous jobs, decodes
// and caches them, and enqueues a reconcile trigger naming the StorageCluster
// object whose status.tasks window they appear in. It lives here rather than in
// the controller package because retrieval, decoding, and caching are the
// subscription's concerns. Writing Kubernetes objects is the reconciler's.
//
// It is the one subscription whose cache does not back an object of its own. A
// task is not mirrored as a custom resource: it is a window on what a cluster
// is doing right now, capped and held in that cluster's status
// (design-storagecluster.md §3.4), so the trigger names the cluster and the
// cache is read whole rather than by task id. What is read by id is the one
// task a CancelTask operation names (§6.3), which is why Lookup is here too.

package subscriptions

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/event"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

// The two terminal conditions of the control plane's own task status enum,
// which is new, running, suspended, or done. A suspended task has not finished:
// it is waiting, which is what keeps it in the window.
const (
	taskStatusDone = "done"
)

// TaskDTO is the operator's view of a control-plane task, matching the fields
// StorageCluster.status.tasks publishes plus the retry count, which is the one
// number that says a task is in trouble rather than merely slow. Unknown fields
// are ignored on decode, so the rest of a task's wire schema costs nothing
// here.
//
// What is absent is worth stating, because the design's Appendix A expects it:
// the control plane's TaskDTO schema carries no creation date and no progress
// figure. Neither can be published, which is why the ordering of the window is
// the control plane's own rather than newest-first, and why ClusterTask carries
// neither field.
type TaskDTO struct {
	ID       string `json:"id"`
	Type     string `json:"function_name"`
	Status   string `json:"status"`
	Result   string `json:"function_result"`
	Canceled bool   `json:"canceled"`
	Retry    int32  `json:"retry"`
}

// Finished reports a task that has reached a terminal outcome and so leaves the
// window. The status enum is the authority: a canceled task and a done one are
// both over, and everything else — new, running, suspended — is a task the
// cluster is still working through.
func (t TaskDTO) Finished() bool { return t.Canceled || t.Status == taskStatusDone }

// TaskSubscription streams a cluster's tasks (one stream per cluster), decodes
// them into an in-memory cache, and enqueues a reconcile trigger naming the
// StorageCluster whose window they belong to. It performs no Kubernetes writes;
// a reconciler consumes its cache and trigger channel.
//
// The control plane knows nothing of Kubernetes object names, so the
// subscription keeps the backend-cluster-id-to-object mapping that the
// StorageCluster controller registers. That is what lets Ingest name an object
// without reading the API — it runs on the stream goroutine and must never
// block on I/O.
type TaskSubscription struct {
	*Cache[TaskDTO]
	ClusterRegistry

	ch chan event.GenericEvent
}

// NewTaskSubscription returns a task subscription. It is told no namespace: the
// window each task appears in belongs to the StorageCluster the task's cluster
// was adopted as, which RegisterCluster supplies.
func NewTaskSubscription() *TaskSubscription {
	return &TaskSubscription{
		Cache:           NewCache(func(t TaskDTO) string { return t.ID }),
		ClusterRegistry: newClusterRegistry(),
		ch:              make(chan event.GenericEvent, 1024),
	}
}

// Name implements cpinformer.Subscription.
func (s *TaskSubscription) Name() string { return "task" }

// Path implements cpinformer.Subscription: tasks are scoped per cluster, so one
// stream is opened per StorageCluster in steady state.
func (s *TaskSubscription) Path(scope cpinformer.Scope) string {
	return fmt.Sprintf("/api/v2/clusters/%s/tasks/", scope[0])
}

// Ingest implements cpinformer.Subscription: it decodes the event into the
// cache, then enqueues one reconcile trigger for the cluster the task belongs
// to. It performs no API I/O, so it never stalls the stream loop.
//
// Every change triggers, including a task reaching a terminal status. That is
// what the window needs: a task leaving it is as much a change to
// status.tasks as one arriving, and it is what the TaskCompleted event is
// raised from.
func (s *TaskSubscription) Ingest(ctx context.Context, ev cpinformer.Event) error {
	return s.Cache.Ingest(ev, func(scope cpinformer.Scope, _ string, _ bool) {
		s.enqueue(ctx, scope)
	})
}

// enqueue pushes a reconcile trigger naming the cluster's StorageCluster
// object, giving up only on shutdown rather than dropping it when the channel
// is full (see [cpinformer.Subscription] on why waiting is the right side to
// err on).
//
// A snapshot reports every task at once and so enqueues once per task. That is
// deliberate rather than wasteful: the channel drains into a workqueue that
// deduplicates by key, so a hundred tasks of one cluster become one reconcile.
func (s *TaskSubscription) enqueue(ctx context.Context, scope cpinformer.Scope) {
	key, ok := s.cluster(scope[0])
	if !ok {
		return
	}
	sc := &simplyblockv1alpha2.StorageCluster{}
	sc.SetNamespace(key.Namespace)
	sc.SetName(key.Name)
	select {
	case s.ch <- event.GenericEvent{Object: sc}:
	case <-ctx.Done():
	}
}

// Triggers is the reconcile-trigger channel, which the reconciler attaches via
// source.Channel. Each event names the StorageCluster whose window moved.
func (s *TaskSubscription) Triggers() <-chan event.GenericEvent { return s.ch }

// Lookup returns the cached task with the given id, or ok=false when the
// control plane no longer reports it. It is what a CancelTask operation reads:
// the operation names one task, and whether it is still running is the whole of
// that action's completion condition.
func (s *TaskSubscription) Lookup(taskID string) (TaskDTO, bool) {
	_, dto, ok := s.Find(taskID)
	return dto, ok
}

var _ cpinformer.Subscription = (*TaskSubscription)(nil)
