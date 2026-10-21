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
	"errors"
	"fmt"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	atlaslvol "github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/mdsfailover"
	"github.com/simplyblock/simplyblock-operator/internal/mdsstate"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// MDSGuestLink is the operator's reach to the guests: their sessions and their
// MDSService.
type MDSGuestLink interface {
	mdsfailover.Guests
	mdsfailover.Sessions
}

// MDSHostGate holds which volumes have every pod's host allowed, as the
// workload coordinator last reported. The takeover leaves a volume that is not
// allowed out of a promotion, since the standby could not connect it. A gate
// never updated allows every volume.
type MDSHostGate struct {
	mu    sync.RWMutex
	hosts mdsstate.HostResult
}

// Update replaces the coordinator's latest result.
func (g *MDSHostGate) Update(h mdsstate.HostResult) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hosts = h
}

// Allowed reports whether the volume's hosts are all allowed.
func (g *MDSHostGate) Allowed(h atlaslvol.VolumeHandle) bool {
	if g == nil {
		return true
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.hosts.Allowed(h)
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
	// Hosts is the coordinator's latest allowed-hosts result. Nil allows all.
	Hosts *MDSHostGate
	// Clock and Metrics default to the wall clock and the registered series.
	Clock   mdsfailover.Clock
	Metrics *mdsfailover.Metrics

	mu        sync.Mutex
	failovers map[client.ObjectKey]*mdsfailover.Failover
}

// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update
// A demoted or rolled guest's pod is deleted, which restarts it as a standby.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=simplyblockdrivers,verbs=get;list;watch

// Reconcile runs one pass of the StatefulSet's takeover.
func (r *MDSFailoverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, req.NamespacedName, &sts); err != nil {
		r.drop(req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if sts.Labels[mdsfailover.ClusterLabel] == "" || !sts.DeletionTimestamp.IsZero() {
		r.drop(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	res, err := r.failoverFor(&sts).Pass(ctx)
	if err != nil {
		// The workqueue's backoff paces the retry.
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: res.RequeueAfter}, nil
}

func (r *MDSFailoverReconciler) drop(key client.ObjectKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failovers, key)
}

// LeadershipAcquired restarts every detection window.
func (r *MDSFailoverReconciler) LeadershipAcquired() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.failovers {
		f.LeadershipAcquired()
	}
}

// SwitchoverTo deletes the active pod of the StatefulSet and prefers target as
// the standby to promote. An empty target leaves the choice to the takeover.
func (r *MDSFailoverReconciler) SwitchoverTo(ctx context.Context, statefulSet, target string) error {
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: statefulSet}, &sts); err != nil {
		return fmt.Errorf("reading StatefulSet %s: %w", statefulSet, err)
	}
	return r.failoverFor(&sts).Switchover(ctx, target)
}

// ActiveOrdinal is the ordinal of the recorded active, and false before the
// first promotion. It is the read half of mdsstate.Switchover.
func (r *MDSFailoverReconciler) ActiveOrdinal(_ context.Context, sts *appsv1.StatefulSet) (int32, bool, error) {
	rec, err := mdsfailover.ParseRecord(sts.Annotations)
	if err != nil {
		return 0, false, err
	}
	if rec.Empty() {
		return 0, false, nil
	}
	ordinal := mdsfailover.Ordinal(rec.Active)
	if ordinal < 0 {
		return 0, false, fmt.Errorf("the recorded active %q is not a StatefulSet pod", rec.Active)
	}
	return int32(ordinal), true, nil
}

// RequestSwitchover moves the active onto an ordinal below keepBelow. It is the
// write half of mdsstate.Switchover, and returns once the request is made.
func (r *MDSFailoverReconciler) RequestSwitchover(ctx context.Context, sts *appsv1.StatefulSet, keepBelow int32) error {
	_, err := r.failoverFor(sts).EnsureActiveBelow(ctx, int(keepBelow))
	return err
}

var _ mdsstate.Switchover = (*MDSFailoverReconciler)(nil)

// failoverFor is the StatefulSet's Failover, built on first use. State of one
// StatefulSet never reaches another's: each has its own detector and timing.
func (r *MDSFailoverReconciler) failoverFor(sts *appsv1.StatefulSet) *mdsfailover.Failover {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := client.ObjectKeyFromObject(sts)
	if f, ok := r.failovers[key]; ok {
		return f
	}
	if r.failovers == nil {
		r.failovers = map[client.ObjectKey]*mdsfailover.Failover{}
	}
	clock := r.Clock
	if clock == nil {
		clock = mdsfailover.RealClock{}
	}
	metrics := r.Metrics
	if metrics == nil {
		metrics = mdsfailover.DefaultMetrics()
	}
	clusterID := sts.Labels[mdsfailover.ClusterLabel]
	exports := &mdsExports{r: r.Exports, clusterID: clusterID, hosts: r.Hosts}
	driverName := ""
	if owner := metav1.GetControllerOf(sts); owner != nil && owner.Kind == "SimplyblockDriver" {
		driverName = owner.Name
	}
	f := mdsfailover.New(mdsfailover.Deps{
		Clock:    clock,
		Observer: &mdsfailover.KubeObserver{Client: r.Client, Namespace: sts.Namespace, StatefulSet: sts.Name, Sessions: r.Guests, Clock: clock},
		Store:    &mdsfailover.KubeStore{Client: r.Client, Key: key},
		Guests:   r.Guests,
		Gateway:  &mdsGateway{r: r.Exports},
		Exports:  exports,
		Pods:     &mdsPods{c: r.Client, namespace: sts.Namespace},
		Events:   &mdsEvents{r: r, namespace: sts.Namespace, driver: driverName, exports: exports},
		Metrics:  metrics,
		Config:   r.Config,
		HostNQN: func(uid string, ordinal int) (string, error) {
			return mdsstate.HostNQN(types.UID(uid), int32(ordinal))
		},
		HostsAllowed: r.Hosts.Allowed,
	})
	r.failovers[key] = f
	return f
}

// SetupWithManager wires the controller. The pods are watched through their
// owner, so a pod turning Ready or terminating wakes the pass, and a periodic
// requeue covers the signals that raise no event, such as a lost link.
func (r *MDSFailoverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Leadership begins when this runnable starts, since the manager starts
	// runnables that need it only once elected.
	if err := mgr.Add(manager.RunnableFunc(func(context.Context) error {
		r.LeadershipAcquired()
		return nil
	})); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.StatefulSet{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			_, ok := obj.GetLabels()[mdsfailover.ClusterLabel]
			return ok
		}))).
		Owns(&corev1.Pod{}).
		Named("mdsfailover").
		Complete(r)
}

// mdsGateway moves the metadata server's one EndpointSlice.
type mdsGateway struct {
	r         *NFSExportReconciler
	mu        sync.Mutex
	withdrawn string
}

// address is the one address the metadata server's slice publishes, "" when it
// is withdrawn or absent.
func (g *mdsGateway) address(ctx context.Context, pod string) (string, error) {
	var eps discoveryv1.EndpointSlice
	key := client.ObjectKey{Namespace: g.r.OperatorNamespace, Name: utils.MDSEndpointSliceName(mdsServiceNameOf(pod))}
	switch err := g.r.Get(ctx, key, &eps); {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("reading the EndpointSlice: %w", err)
	}
	for _, e := range eps.Endpoints {
		if len(e.Addresses) > 0 {
			return e.Addresses[0], nil
		}
	}
	return "", nil
}

// clusterIP is the metadata server's Service address, "" while it has none.
func (g *mdsGateway) clusterIP(ctx context.Context, pod string) string {
	var svc corev1.Service
	key := client.ObjectKey{Namespace: g.r.OperatorNamespace, Name: mdsServiceNameOf(pod)}
	if err := g.r.Get(ctx, key, &svc); err != nil {
		return ""
	}
	return svc.Spec.ClusterIP
}

// Withdraw empties the slice and has the nodes forget the flows to the address
// it held. An empty slice asks for nothing.
func (g *mdsGateway) Withdraw(ctx context.Context, pod string) error {
	current, err := g.address(ctx, pod)
	if err != nil || current == "" {
		return err
	}
	if err := g.r.reconcileExportEndpointSlice(ctx, pod, ""); err != nil {
		return fmt.Errorf("withdrawing %s: %w", current, err)
	}
	g.mu.Lock()
	g.withdrawn = current
	g.mu.Unlock()
	g.forget(ctx, pod, current)
	return nil
}

// Serve points the slice at the pod IP and has the nodes forget the flows to the
// address it replaces. The same address coming back cancels the forget still
// waiting instead of repeating it.
func (g *mdsGateway) Serve(ctx context.Context, pod, ip string) error {
	svcIP, err := g.r.reconcileMDSServiceObject(ctx, pod)
	if err != nil {
		return err
	}
	current, err := g.address(ctx, pod)
	if err != nil {
		return err
	}
	g.mu.Lock()
	withdrawn := g.withdrawn
	g.mu.Unlock()
	if current == ip {
		return nil
	}
	if err := g.r.reconcileExportEndpointSlice(ctx, pod, ip); err != nil {
		return fmt.Errorf("pointing the EndpointSlice at %s: %w", ip, err)
	}
	g.mu.Lock()
	g.withdrawn = ""
	g.mu.Unlock()
	old := current
	if old == "" {
		old = withdrawn
	}
	switch {
	case old == "" || svcIP == "" || g.r.Flows == nil:
	case old == ip:
		g.r.Flows.Cancel(svcIP, old)
	default:
		g.r.Flows.Forget(svcIP, old)
	}
	return nil
}

func (g *mdsGateway) forget(ctx context.Context, pod, ip string) {
	if svc := g.clusterIP(ctx, pod); svc != "" && g.r.Flows != nil {
		g.r.Flows.Forget(svc, ip)
	}
}

// mdsExports is the storage cluster's NFSExports as the takeover sees them.
type mdsExports struct {
	r         *NFSExportReconciler
	clusterID string
	hosts     *MDSHostGate
}

// live is every export of the cluster that a takeover has to place: not being
// deleted, not Degraded (nothing ever serves it), and on a volume whose hosts
// are all allowed, since a standby cannot connect any other.
func (e *mdsExports) live(ctx context.Context) ([]simplyblockv1alpha2.NFSExport, error) {
	var list simplyblockv1alpha2.NFSExportList
	if err := e.r.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing exports: %w", err)
	}
	var out []simplyblockv1alpha2.NFSExport
	for _, x := range list.Items {
		h, ok := atlaslvol.ParseHandle(atlaslvol.VolumeHandle(x.Spec.VolumeRef))
		if !ok || h.ClusterID != e.clusterID || !x.DeletionTimestamp.IsZero() ||
			x.Status.Phase == simplyblockv1alpha2.NFSExportPhaseDegraded ||
			!e.hosts.Allowed(atlaslvol.VolumeHandle(x.Spec.VolumeRef)) {
			continue
		}
		out = append(out, x)
	}
	return out, nil
}

func (e *mdsExports) List(ctx context.Context) ([]mdsfailover.ExportState, error) {
	live, err := e.live(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mdsfailover.ExportState, 0, len(live))
	for _, x := range live {
		out = append(out, mdsfailover.ExportState{
			Key: client.ObjectKeyFromObject(&x), Pod: x.Status.MDSPodName, AssembledBy: x.Status.AssembledBy,
		})
	}
	return out, nil
}

func (e *mdsExports) Specs(ctx context.Context, hostNQN string) ([]nfsexport.Spec, error) {
	live, err := e.live(ctx)
	if err != nil {
		return nil, err
	}
	var specs []nfsexport.Spec
	for i := range live {
		if live[i].Status.MDSPodName == "" {
			continue
		}
		spec, err := specFor(&live[i], hostNQN)
		switch {
		case errors.Is(err, nfsexport.ErrInvalidSpec):
			// Not assemblable yet: the guest assembles it when its binding is.
		case err != nil:
			return nil, err
		default:
			specs = append(specs, spec)
		}
	}
	return specs, nil
}

// Rebind writes status.mdsPodName and nothing else, against a fresh copy.
func (e *mdsExports) Rebind(ctx context.Context, export types.NamespacedName, pod string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.NFSExport
		if err := e.r.Get(ctx, export, &fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		if fresh.Status.MDSPodName == pod {
			return nil
		}
		fresh.Status.MDSPodName = pod
		return e.r.Status().Update(ctx, &fresh)
	})
}

// mdsPods deletes a pod of the metadata server.
type mdsPods struct {
	c         client.Client
	namespace string
}

func (p *mdsPods) Delete(ctx context.Context, pod string) error {
	err := p.c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: p.namespace}})
	return client.IgnoreNotFound(err)
}

// mdsEvents emits a takeover's events on the SimplyblockDriver and on the
// cluster's exports.
type mdsEvents struct {
	r         *MDSFailoverReconciler
	namespace string
	driver    string
	exports   *mdsExports
}

func (e *mdsEvents) Driver(eventType, reason, message string) {
	if e.r.Recorder == nil || e.driver == "" {
		return
	}
	var d simplyblockv1alpha2.SimplyblockDriver
	if err := e.r.Get(context.Background(), client.ObjectKey{Namespace: e.namespace, Name: e.driver}, &d); err != nil {
		return
	}
	e.r.Recorder.Eventf(&d, nil, eventType, reason, reason, "%s", message)
}

func (e *mdsEvents) Exports(eventType, reason, message string) {
	if e.r.Recorder == nil {
		return
	}
	live, err := e.exports.live(context.Background())
	if err != nil {
		return
	}
	for i := range live {
		e.r.Recorder.Eventf(&live[i], nil, eventType, reason, reason, "%s", message)
	}
}

// MDSServingGate says which pod serves a metadata server, once the takeover has
// made one active.
type MDSServingGate interface {
	// ServingPod is the active pod of the StatefulSet when it is serving at the
	// record's epoch, and otherwise "" with why.
	ServingPod(ctx context.Context, statefulSet string) (pod, why string, err error)
}

// mdsServingGate is the MDSServingGate over the record and the guests.
type mdsServingGate struct {
	Client    client.Client
	Namespace string
	Guests    mdsfailover.Guests
}

func (g *mdsServingGate) ServingPod(ctx context.Context, statefulSet string) (string, string, error) {
	var sts appsv1.StatefulSet
	switch err := g.Client.Get(ctx, client.ObjectKey{Namespace: g.Namespace, Name: statefulSet}, &sts); {
	case apierrors.IsNotFound(err):
		return "", fmt.Sprintf("StatefulSet %s does not exist yet", statefulSet), nil
	case err != nil:
		return "", "", fmt.Errorf("reading StatefulSet %s: %w", statefulSet, err)
	}
	rec, err := mdsfailover.ParseRecord(sts.Annotations)
	if err != nil {
		return "", "", err
	}
	if rec.Empty() {
		return "", "waiting for the first promotion of the metadata server", nil
	}
	d, err := g.Guests.Describe(ctx, rec.Active)
	switch {
	case err != nil:
		return "", fmt.Sprintf("waiting for the metadata server %s to link: %v", rec.Active, err), nil
	case d.Role != nfsexportrpc.RoleActive || d.Epoch != rec.Epoch:
		return "", fmt.Sprintf("waiting for %s to be active at epoch %d, it is %s at epoch %d",
			rec.Active, rec.Epoch, d.Role, d.Epoch), nil
	}
	return rec.Active, "", nil
}
