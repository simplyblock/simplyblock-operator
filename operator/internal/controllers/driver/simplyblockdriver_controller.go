// The reconciler that turns a SimplyblockDriver into a running CSI driver.
//
// It applies the deployment's eighteen objects, keeps the two ownership
// mechanisms of ownership.go on them, and removes the cluster-scoped half
// through a finalizer, since nothing collects those on its behalf.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §4.

package driver

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// driverFinalizer is what gives the controller a pass to delete the
	// cluster-scoped objects with, since the garbage collector will not.
	driverFinalizer = "storage.simplyblock.io/simplyblockdriver-finalizer"

	// driverResyncInterval is how often a healthy deployment is re-examined.
	// The plugins are watched, so this is a backstop against a missed event
	// rather than the way progress is made.
	driverResyncInterval = 2 * time.Minute

	// fieldOwner is the field manager this controller applies under. Adoption
	// takes ownership from Helm's manager under this name, so it has to be
	// stable across releases.
	fieldOwner = client.FieldOwner("simplyblock-operator")
)

// Event reasons.
const (
	reasonDuplicateDriver   = "DuplicateDriver"
	reasonDriverReady       = "DriverReady"
	reasonDriverDegraded    = "DriverDegraded"
	reasonDriverUnavailable = "DriverUnavailable"
	reasonNoMatchingWorkers = "NoMatchingWorkers"
	reasonDriverAdopted     = "DriverAdopted"
	reasonAdoptionRefused   = "AdoptionRefused"
)

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=simplyblockdrivers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=simplyblockdrivers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=simplyblockdrivers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=daemonsets;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete;escalate;bind
// +kubebuilder:rbac:groups=storage.k8s.io,resources=csidrivers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list;watch;create;update;patch;delete

// SimplyblockDriverReconciler applies the CSI driver deployment.
type SimplyblockDriverReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

func (r *SimplyblockDriverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var d simplyblockv1alpha2.SimplyblockDriver
	if err := r.Get(ctx, req.NamespacedName, &d); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !d.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &d)
	}

	// The singleton's controller half, and it runs before the finalizer is
	// taken. The webhook denies a second object at admission, and this is what
	// holds when the webhook was not serving.
	//
	// The order matters more than it looks. Cluster-scoped names are derived
	// from the object's name alone and carry no namespace, so a second driver
	// of the same name in another namespace derives the holder's twelve
	// cluster-scoped objects exactly. A finalizer on that object would delete
	// the running deployment's RBAC and registration when somebody removed the
	// duplicate, which is the opposite of what removing a duplicate should do.
	holder, err := r.deploymentHolder(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if holder.Namespace != d.Namespace || holder.Name != d.Name {
		message := fmt.Sprintf(
			"a Kubernetes cluster holds one SimplyblockDriver, and %s/%s holds it",
			holder.Namespace, holder.Name)
		r.event(&d, corev1.EventTypeWarning, reasonDuplicateDriver, message)
		return ctrl.Result{}, r.setStatus(ctx, &d, simplyblockv1alpha2.SimplyblockDriverPhaseInstalling, message)
	}

	if !controllerutil.ContainsFinalizer(&d, driverFinalizer) {
		controllerutil.AddFinalizer(&d, driverFinalizer)
		if err := r.Update(ctx, &d); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Adoption refuses on the one thing the spec cannot change. The counts are
	// still published, because the plugins are running and what is blocked is
	// the handover rather than the driver.
	refusal, refused, err := r.adoptionRefusal(ctx, &d)
	if err != nil {
		return ctrl.Result{}, err
	}
	if refused {
		r.event(&d, corev1.EventTypeWarning, reasonAdoptionRefused, refusal)
		h, err := r.observe(ctx, &d)
		if err != nil {
			return ctrl.Result{}, err
		}
		h.phase = simplyblockv1alpha2.SimplyblockDriverPhaseInstalling
		h.message = refusal
		return ctrl.Result{RequeueAfter: driverResyncInterval}, r.setHealth(ctx, &d, h)
	}

	// TODO(simplyblockdriver): publish status.version and compare it against the
	// control plane's, which is design-simplyblockdriver.md §5 and the reason
	// the kind exists. Two things are missing and neither is in this repository:
	//
	//   - GET /_meta/version on the management API, so that
	//     ControlPlane.status.version has anything to publish. Design
	//     §5.3 records it, and until it lands half a comparison reports no skew
	//     where it cannot tell.
	//   - https://install.simplyblock.io/releases.yaml, which is where a driver
	//     release declares the control planes it works against (§5.1). The
	//     comparison is a lookup in that document rather than a rule over
	//     version numbers, because the components ship on their own cadences.
	//
	// What is implementable before either arrives is the fetch, the cache, and
	// the pattern matching, which is worth nothing on its own: with no
	// control-plane version to test against, every deployment reports no skew.
	// So this waits on the endpoint rather than shipping a comparison that
	// always says the same thing.
	//
	// The test plan's U-20 to U-25 and U-45 to U-53 are the rows this owes.
	met, err := r.apply(ctx, &d)
	if err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("applied the driver deployment", "driver", names(&d).csiDriver, "adopted", met)

	if err := r.recordOrigin(ctx, &d, met); err != nil {
		return ctrl.Result{}, err
	}

	h, err := r.observe(ctx, &d)
	if err != nil {
		return ctrl.Result{}, err
	}

	// The event marks the arrival rather than the state, so a deployment that
	// stays Degraded says so once instead of on every resync.
	if h.phase != d.Status.Phase {
		if eventType, reason, ok := eventFor(h); ok {
			r.event(&d, eventType, reason, h.message)
		}
	}

	if err := r.setHealth(ctx, &d, h); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: driverResyncInterval}, nil
}

// observe reads the two workloads and the registration back, which is what §4.2
// derives the phase from. A workload that is not there yet is not an error: the
// apply above created it and the cache has not caught up.
func (r *SimplyblockDriverReconciler) observe(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver,
) (health, error) {
	n := names(d)

	var node appsv1.DaemonSet
	nodePtr := &node
	if err := r.Get(ctx, client.ObjectKey{Namespace: d.Namespace, Name: n.nodeDaemonSet}, &node); err != nil {
		if !errors.IsNotFound(err) {
			return health{}, err
		}
		nodePtr = nil
	}

	var controller appsv1.StatefulSet
	controllerPtr := &controller
	if err := r.Get(ctx,
		client.ObjectKey{Namespace: d.Namespace, Name: n.controllerStatefulSet}, &controller); err != nil {
		if !errors.IsNotFound(err) {
			return health{}, err
		}
		controllerPtr = nil
	}

	var registration storagev1.CSIDriver
	registered := true
	if err := r.Get(ctx, client.ObjectKey{Name: n.csiDriver}, &registration); err != nil {
		if !errors.IsNotFound(err) {
			return health{}, err
		}
		registered = false
	}

	return derive(nodePtr, controllerPtr, registered), nil
}

// event records what the reconcile decided, so that a refusal to act is
// visible to somebody reading the object rather than only in a log.
func (r *SimplyblockDriverReconciler) event(
	d *simplyblockv1alpha2.SimplyblockDriver, eventType, reason, message string,
) {
	if r.Recorder == nil {
		return
	}
	// The action is the reason, as every other recorder call in this operator
	// passes it. It is a required field, and the phase would be empty on the
	// first event an object ever emits.
	r.Recorder.Eventf(d, nil, eventType, reason, reason, "%s", message)
}

// desired is every object this deployment owns, in the order it is applied:
// the accounts and configuration first, then the RBAC that names the accounts,
// then the workloads that mount the configuration, and the registration last.
func (r *SimplyblockDriverReconciler) desired(d *simplyblockv1alpha2.SimplyblockDriver) []client.Object {
	objects := make([]client.Object, 0, 18)

	for _, sa := range serviceAccounts(d) {
		objects = append(objects, sa)
	}
	for _, cm := range configMaps(d) {
		objects = append(objects, cm)
	}
	for _, cr := range clusterRoles(d) {
		objects = append(objects, cr)
	}
	for _, crb := range clusterRoleBindings(d) {
		objects = append(objects, crb)
	}
	objects = append(objects, nodeDaemonSet(d), controllerStatefulSet(d), csiDriver(d))
	if snapshotsEnabled(d) {
		objects = append(objects, volumeSnapshotClass(d))
	}
	return objects
}

// TODO(simplyblockdriver): verify the handover before stripping the release's
// claim, which is design §4.3's step 6 and design-api-upgrade.md §12.4's, where
// it calls the verification steps load bearing rather than ceremony. Nothing
// here reads back that the controller reference or the managed-by label
// actually landed, so an
// apply that silently lost ownership is followed by a patch that removes Helm's,
// leaving the object owned by nobody.

// apply writes every object of the set and reports whether any of them was
// already there. The apply is a server-side apply under a stable field manager,
// which is what lets the same call create an object that is absent and take the
// fields of one a Helm release left behind.
func (r *SimplyblockDriverReconciler) apply(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver,
) (metExisting bool, err error) {
	for _, obj := range r.desired(d) {
		fromHelm, existed, err := r.inspectExisting(ctx, obj)
		if err != nil {
			return false, err
		}
		metExisting = metExisting || existed

		// Before anything else takes it over, make the object one Helm will not
		// prune. Helm reads the annotation off the live object, so writing it in
		// the same apply is enough.
		if fromHelm {
			keepThroughHelm(obj)
		}

		if err := setOwnership(d, obj, r.Scheme); err != nil {
			return false, fmt.Errorf("set ownership on %T %s: %w", obj, obj.GetName(), err)
		}
		cfg, err := r.applyConfiguration(obj)
		if err != nil {
			return false, fmt.Errorf("encode %T %s for apply: %w", obj, obj.GetName(), err)
		}
		if err := r.Apply(ctx, cfg, fieldOwner, client.ForceOwnership); err != nil {
			return false, fmt.Errorf("apply %T %s: %w", obj, obj.GetName(), err)
		}

		// Only once the apply has landed, so that an object is never left
		// carrying neither the release's claim nor this controller's.
		if fromHelm {
			patch := client.RawPatch(types.MergePatchType, helmMetadataRemovalPatch())
			if err := r.Patch(ctx, obj, patch); err != nil {
				return false, fmt.Errorf("remove Helm metadata from %T %s: %w", obj, obj.GetName(), err)
			}
		}
	}
	return metExisting, nil
}

// inspectExisting reads back what is already in the cluster under an object's
// name, without disturbing the object about to be applied.
func (r *SimplyblockDriverReconciler) inspectExisting(
	ctx context.Context, obj client.Object,
) (fromHelm, existed bool, err error) {
	current, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return false, false, fmt.Errorf("%T is not a client.Object", obj)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
		if errors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("read %T %s: %w", obj, obj.GetName(), err)
	}
	return carriesHelmMetadata(current), true, nil
}

// TODO(simplyblockdriver): decide who seeds a SimplyblockDriver's spec from a
// running deployment, which is design §4.3's "The spec is seeded from what is
// running" table. Nothing does it: the chart no longer renders the object, and
// there is no installer in this repository, so on an upgraded cluster somebody
// writes it by hand. Omitting driverName then defaults it, and the refusal
// below catches that rather than orphaning volumes, but catching it is not the
// same as translating it. design-api-upgrade.md §13.1 covers only the values
// renaming, so the table is unowned in both documents.

// adoptionRefusal compares the driver name a running node plugin registers
// under against the one the spec declares. The two disagree only where a
// deployment registered under a name this object cannot be edited to match,
// since spec.driverName is immutable, and applying over it would leave the
// object declaring one driver while the cluster attaches volumes through
// another.
func (r *SimplyblockDriverReconciler) adoptionRefusal(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver,
) (message string, refused bool, err error) {
	var node appsv1.DaemonSet
	key := client.ObjectKey{Namespace: d.Namespace, Name: names(d).nodeDaemonSet}
	if err := r.Get(ctx, key, &node); err != nil {
		if errors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}

	if what, unsupported := unsupportedConfiguration(&node); unsupported {
		return fmt.Sprintf(
			"the running node plugin is configured with %s, which this kind has no field for; "+
				"adopting it would reconcile that configuration away, so the handover stops here "+
				"rather than turning it off",
			what), true, nil
	}

	running, found := runningDriverName(&node)
	if !found || running == names(d).csiDriver {
		return "", false, nil
	}
	return fmt.Sprintf(
		"the running node plugin registers the driver as %q and spec.driverName is %q; "+
			"spec.driverName is immutable, so this deployment cannot be adopted under it, "+
			"and every PersistentVolume already provisioned records the running name",
		running, names(d).csiDriver), true, nil
}

// recordOrigin writes where the deployment came from, once. It is not revised
// afterward, because what it records is the state the first reconcile found
// rather than what the controller did most recently.
func (r *SimplyblockDriverReconciler) recordOrigin(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver, metExisting bool,
) error {
	if d.Status.Origin != "" {
		return nil
	}

	origin := simplyblockv1alpha2.SimplyblockDriverOriginCreated
	if metExisting {
		origin = simplyblockv1alpha2.SimplyblockDriverOriginAdopted
		r.event(d, corev1.EventTypeNormal, reasonDriverAdopted,
			"took over a deployment that was already running rather than creating one")
	}
	return r.writeStatus(ctx, d, func(status *simplyblockv1alpha2.SimplyblockDriverStatus) {
		status.Origin = origin
	})
}

// applyConfiguration turns a built object into the shape a server-side apply
// takes. The apiVersion and kind have to be on the wire for an apply, and a
// typed object built in Go carries an empty TypeMeta, so the kind is resolved
// from the scheme rather than written by hand at each call site.
func (r *SimplyblockDriverReconciler) applyConfiguration(obj client.Object) (runtime.ApplyConfiguration, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return client.ApplyConfigurationFromUnstructured(u), nil
	}

	gvk, err := apiutil.GVKForObject(obj, r.Scheme)
	if err != nil {
		return nil, err
	}
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: content}
	u.SetGroupVersionKind(gvk)
	// A built object has no status and an apply that carries an empty one
	// claims ownership of a field the controller does not set.
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	return client.ApplyConfigurationFromUnstructured(u), nil
}

// finalize removes what the garbage collector will not, then releases the
// finalizer. The namespaced children go with the object through their owner
// references, so only the cluster-scoped half is deleted here, and only where
// this controller's label says it may.
func (r *SimplyblockDriverReconciler) finalize(ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver) error {
	if !controllerutil.ContainsFinalizer(d, driverFinalizer) {
		return nil
	}

	// Deleting a driver that never held the deployment must take nothing with
	// it. The check is repeated here rather than trusted from the reconcile
	// that added the finalizer, because an object may carry one from before
	// that ordering existed, and the cost of being wrong is the running
	// deployment's RBAC and registration.
	holder, err := r.deploymentHolder(ctx)
	if err != nil {
		return err
	}
	if holder.Name != "" && (holder.Namespace != d.Namespace || holder.Name != d.Name) {
		controllerutil.RemoveFinalizer(d, driverFinalizer)
		return r.Update(ctx, d)
	}

	for _, obj := range r.ownedClusterScoped(d) {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("read %T %s before deleting it: %w", obj, obj.GetName(), err)
		}
		if !mayDelete(obj) {
			// Somebody else's, or nobody's. A cluster-scoped object is shared
			// ground and deleting one this controller did not mark is deleting
			// another deployment's RBAC.
			continue
		}
		if err := r.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete %T %s: %w", obj, obj.GetName(), err)
		}
	}

	controllerutil.RemoveFinalizer(d, driverFinalizer)
	return r.Update(ctx, d)
}

// ownedClusterScoped is everything the finalizer has to consider: the
// cluster-scoped half of the object set, and the snapshot class whether or not
// the toggle currently asks for it. A class applied while snapshots were
// enabled is one nothing else would ever remove.
func (r *SimplyblockDriverReconciler) ownedClusterScoped(
	d *simplyblockv1alpha2.SimplyblockDriver,
) []client.Object {
	var out []client.Object
	for _, obj := range r.desired(d) {
		if obj.GetNamespace() == "" {
			out = append(out, obj)
		}
	}
	if !snapshotsEnabled(d) {
		out = append(out, volumeSnapshotClass(d))
	}
	return out
}

// deploymentHolder is the object that owns the deployment: the oldest in the
// Kubernetes cluster, with namespace and name breaking a tie. The webhook picks
// the same one from the same list, so the two never disagree about which object
// is the second.
func (r *SimplyblockDriverReconciler) deploymentHolder(ctx context.Context) (client.ObjectKey, error) {
	var drivers simplyblockv1alpha2.SimplyblockDriverList
	if err := r.List(ctx, &drivers); err != nil {
		return client.ObjectKey{}, err
	}
	if len(drivers.Items) == 0 {
		return client.ObjectKey{}, nil
	}
	oldest := DeploymentHolder(drivers.Items)
	return client.ObjectKey{Namespace: oldest.Namespace, Name: oldest.Name}, nil
}

// setStatus writes the phase and the observed generation together, so that a
// reader can tell whether the phase was computed from the spec they are looking
// at.
func (r *SimplyblockDriverReconciler) setStatus(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver,
	phase simplyblockv1alpha2.SimplyblockDriverPhase, message string,
) error {
	return r.writeStatus(ctx, d, func(status *simplyblockv1alpha2.SimplyblockDriverStatus) {
		status.Phase = phase
		status.Message = message
	})
}

// TODO(simplyblockdriver): publish design §6.2's four gauges,
// simplyblock_simplyblockdriver_{version_info,nodes_ready_count,
// nodes_expected_count,controller_ready_state}. The three that are not the
// version are computable from the health below and need only a collector; the
// version one waits on §5.

// setHealth writes the phase together with the counts it is explained by, since
// a phase a reader cannot check against the numbers behind it sends them to
// kubectl describe to learn which worker is short.
func (r *SimplyblockDriverReconciler) setHealth(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver, h health,
) error {
	return r.writeStatus(ctx, d, func(status *simplyblockv1alpha2.SimplyblockDriverStatus) {
		status.Phase = h.phase
		status.Message = h.message
		status.NodesReady = h.nodesReady
		status.NodesTotal = h.nodesTotal
		status.ControllerReady = h.controllerReady
	})
}

func (r *SimplyblockDriverReconciler) writeStatus(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver,
	mutate func(*simplyblockv1alpha2.SimplyblockDriverStatus),
) error {
	key := client.ObjectKeyFromObject(d)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current simplyblockv1alpha2.SimplyblockDriver
		if err := r.Get(ctx, key, &current); err != nil {
			return client.IgnoreNotFound(err)
		}
		mutate(&current.Status)
		current.Status.ObservedGeneration = current.Generation
		if err := r.Status().Update(ctx, &current); err != nil {
			return err
		}
		d.Status = current.Status
		return nil
	})
	return err
}

func (r *SimplyblockDriverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.SimplyblockDriver{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&appsv1.StatefulSet{}).
		Named("simplyblockdriver").
		Complete(r)
}
