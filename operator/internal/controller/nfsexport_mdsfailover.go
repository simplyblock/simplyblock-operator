// The takeover of a storage cluster's metadata server, as the operator runs it:
// a reconciler of the MDS StatefulSet and the adapters that give the decisions
// of internal/mdsfailover their Kubernetes, EndpointSlice, and export hands
// (design-pnfs-mds-standby.md §7).
//
// It lives beside the NFSExport reconciler because the takeover moves what that
// reconciler owns: each export's status.mdsPodName, and the metadata server's
// Service and EndpointSlice. The decisions themselves are in mdsfailover, where
// they are tested with fakes of every interface used here.

package controller

import (
	"context"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/simplyblock-operator/internal/mdsfailover"
)

// MDSGuestLink is the operator's reach to the guests: their sessions and their
// MDSService.
type MDSGuestLink interface {
	mdsfailover.Guests
	mdsfailover.Sessions
}

// MDSFailoverReconciler runs the takeover of each metadata server StatefulSet.
type MDSFailoverReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// Guests reaches each pod's MDSService.
	Guests MDSGuestLink
	// Exports supplies the Service and EndpointSlice helpers, the flow
	// forgetting, and the status writes the takeover shares with the export
	// reconciler.
	Exports           *NFSExportReconciler
	OperatorNamespace string
	Config            mdsfailover.Config
	// Clock and Metrics default to the wall clock and the registered series.
	Clock   mdsfailover.Clock
	Metrics *mdsfailover.Metrics

	mu        sync.Mutex
	failovers map[client.ObjectKey]*mdsfailover.Failover
}

// Reconcile runs one pass of the StatefulSet's takeover.
func (r *MDSFailoverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// LeadershipAcquired restarts every detection window.
func (r *MDSFailoverReconciler) LeadershipAcquired() {}

// SwitchoverTo deletes the active pod of the StatefulSet and prefers target as
// the standby to promote.
func (r *MDSFailoverReconciler) SwitchoverTo(ctx context.Context, statefulSet, target string) error {
	return nil
}

// ActiveOrdinal is the ordinal of the recorded active, and false before the
// first promotion. It is the read half of mdsstate.Switchover.
func (r *MDSFailoverReconciler) ActiveOrdinal(ctx context.Context, sts *appsv1.StatefulSet) (int32, bool, error) {
	return 0, false, nil
}

// RequestSwitchover moves the active onto an ordinal below keepBelow. It is the
// write half of mdsstate.Switchover.
func (r *MDSFailoverReconciler) RequestSwitchover(ctx context.Context, sts *appsv1.StatefulSet, keepBelow int32) error {
	return nil
}

// SetupWithManager wires the controller.
func (r *MDSFailoverReconciler) SetupWithManager(mgr ctrl.Manager) error { return nil }

// mdsGateway moves the metadata server's one EndpointSlice.
type mdsGateway struct {
	r *NFSExportReconciler
}

func (g *mdsGateway) Withdraw(ctx context.Context, pod string) error { return nil }

func (g *mdsGateway) Serve(ctx context.Context, pod, ip string) error { return nil }

// mdsExports is the storage cluster's NFSExports as the takeover sees them.
type mdsExports struct {
	r         *NFSExportReconciler
	clusterID string
}

func (e *mdsExports) List(ctx context.Context) ([]mdsfailover.ExportState, error) { return nil, nil }

func (e *mdsExports) Specs(ctx context.Context, hostNQN string) ([]nfsexport.Spec, error) {
	return nil, nil
}

func (e *mdsExports) Rebind(ctx context.Context, export types.NamespacedName, pod string) error {
	return nil
}

// MDSServingGate says which pod serves a metadata server, once the takeover has
// made one active.
type MDSServingGate interface {
	// ServingPod is the active pod of the StatefulSet when it is serving at the
	// record's epoch, and otherwise "" with why.
	ServingPod(ctx context.Context, statefulSet string) (pod, why string, err error)
}

// mdsEvents emits a takeover's events on the SimplyblockDriver and on the
// cluster's exports.
type mdsEvents struct {
	r         *MDSFailoverReconciler
	namespace string
	driver    string
	exports   *mdsExports
}

func (e *mdsEvents) Driver(eventType, reason, message string) {}

func (e *mdsEvents) Exports(eventType, reason, message string) {}

// mdsServingGate is the MDSServingGate over the record and the guests.
type mdsServingGate struct {
	Client    client.Client
	Namespace string
	Guests    mdsfailover.Guests
}

func (g *mdsServingGate) ServingPod(ctx context.Context, statefulSet string) (string, string, error) {
	return "", "", nil
}
