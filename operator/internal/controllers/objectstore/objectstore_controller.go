// ObjectStoreReconciler drives one S3 gateway's workload from creation to
// Ready: the Deployment, the ClusterIP Service, and that Service's serving
// certificate (design-objectstore.md). No finalizer: every owned object is
// garbage-collected by Kubernetes on delete (§4.2), and nothing outside them
// needs cleanup.

package objectstore

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/statemachine"
	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// osPhase is an alias so the graph below reads as a table. Named osPhase
// rather than phase: S3BucketReconciler's own phase/phasePending/... already
// claim those names in this package (s3bucket_controller.go), one kind each.
type osPhase = objectstorev1alpha1.ObjectStorePhase

const (
	osPhasePending      = objectstorev1alpha1.ObjectStorePhasePending
	osPhaseProvisioning = objectstorev1alpha1.ObjectStorePhaseProvisioning
	osPhaseReady        = objectstorev1alpha1.ObjectStorePhaseReady
	osPhaseDegraded     = objectstorev1alpha1.ObjectStorePhaseDegraded
)

// Condition types this reconciler sets, design §4.2's own names.
const (
	conditionDeploymentAvailable = "DeploymentAvailable"
	conditionCertificateReady    = "CertificateReady"
)

// The controller's cadence, in one place so it can be read and tuned.
const (
	// How long Provisioning may wait on the Deployment reporting Available
	// and the certificate reporting Ready, before the ObjectStore degrades
	// (design §6).
	objectStoreProvisioningDeadline = 5 * time.Minute
	// Come back with a re-read object after a write that bumped
	// resourceVersion, so status is not written over a copy already stale.
	objectStoreFreshCopyRequeue = time.Second
	// How many ObjectStores reconcile at once. Each one is independent
	// (design §7.3), so this is a throughput knob, not a correctness one.
	objectStoreConcurrency = 8
)

// ObjectStoreReconciler reconciles ObjectStore objects.
type ObjectStoreReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=objectstore.simplyblock.io,resources=objectstores,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=objectstore.simplyblock.io,resources=objectstores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;delete

func (r *ObjectStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("objectstore", req.NamespacedName)

	var store objectstorev1alpha1.ObjectStore
	if err := r.Get(ctx, req.NamespacedName, &store); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Degraded is terminal on purpose, the same reason S3Bucket's and
	// NFSExport's own are: it is reached by refusing to act, and retrying
	// the refusal on a timer would bury the event that explains it.
	if store.Status.Phase == osPhaseDegraded {
		return ctrl.Result{}, nil
	}

	sm, err := r.restore(ctx, &store)
	if err != nil {
		r.event(&store, corev1.EventTypeWarning, "PhaseUnrecognized",
			fmt.Sprintf("cannot restore phase %q", store.Status.Phase))
		return ctrl.Result{}, err
	}
	defer sm.Close()

	if sm.IsTerminal() {
		// Ready has no outgoing edge: a routine spec change (replicas,
		// image, placement) reconciles the owned objects in place and
		// stays Ready (design §6's own failure-table row for
		// spec.replicas), it never returns to Provisioning or Pending.
		if sm.CurrentState() == osPhaseReady {
			return r.reconcileReady(ctx, &store)
		}
		return ctrl.Result{}, nil
	}

	if sm.TimeoutReached() {
		reason, message, conditionType := r.provisioningFailure(ctx, &store)
		return ctrl.Result{}, r.toDegraded(ctx, &store, sm, reason, message, conditionType)
	}

	switch sm.CurrentState() {
	case osPhasePending:
		return r.reconcilePending(ctx, &store, sm)
	case osPhaseProvisioning:
		return r.reconcileProvisioning(ctx, &store, sm, logger)
	default:
		// restore refuses to build a machine for an unknown phase, so this
		// is a known phase with nothing to do in it.
		return ctrl.Result{}, nil
	}
}

// reconcilePending creates the Deployment, the Service, and the certificate,
// then enters Provisioning. Nothing here needs an OnEnter hook because
// Pending is the graph's own starting point.
func (r *ObjectStoreReconciler) reconcilePending(
	ctx context.Context, store *objectstorev1alpha1.ObjectStore, sm *statemachine.Machine[osPhase],
) (ctrl.Result, error) {
	if err := r.applyOwnedObjects(ctx, store); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating the gateway's Deployment, Service, and certificate: %w", err)
	}

	if err := sm.TransitionTo(ctx, osPhaseProvisioning); err != nil {
		return ctrl.Result{}, fmt.Errorf("transition to Provisioning: %w", err)
	}
	if err := r.writeStatus(ctx, store, func(s *objectstorev1alpha1.ObjectStoreStatus) {
		s.Phase = osPhaseProvisioning
		setObjectStoreDeadline(s, sm)
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: objectStoreFreshCopyRequeue}, nil
}

// reconcileProvisioning re-applies the owned objects (idempotent, and what
// resuming after a restart needs: the Deployment may already exist) and
// waits for the Deployment to report Available and the certificate to
// report Ready.
func (r *ObjectStoreReconciler) reconcileProvisioning(
	ctx context.Context, store *objectstorev1alpha1.ObjectStore, sm *statemachine.Machine[osPhase], logger logr,
) (ctrl.Result, error) {
	if err := r.applyOwnedObjects(ctx, store); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling the gateway's Deployment, Service, and certificate: %w", err)
	}

	deploymentAvailable, readyReplicas, err := r.deploymentAvailable(ctx, store)
	if err != nil {
		return ctrl.Result{}, err
	}
	certificateReady, err := r.certificateReady(ctx, store)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !deploymentAvailable || !certificateReady {
		return ctrl.Result{RequeueAfter: objectStoreFreshCopyRequeue}, nil
	}

	logger.Info("gateway Deployment Available and certificate Ready, entering Ready")
	if err := sm.TransitionTo(ctx, osPhaseReady); err != nil {
		return ctrl.Result{}, fmt.Errorf("transition to Ready: %w", err)
	}
	if err := r.writeStatus(ctx, store, func(s *objectstorev1alpha1.ObjectStoreStatus) {
		s.Phase = osPhaseReady
		s.ReadyReplicas = readyReplicas
		s.PhaseDeadline = nil
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: conditionDeploymentAvailable, Status: metav1.ConditionTrue, Reason: "MinimumReplicasAvailable",
		})
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: conditionCertificateReady, Status: metav1.ConditionTrue, Reason: "CertificateIssued",
		})
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.event(store, corev1.EventTypeNormal, "ObjectStoreReady", fmt.Sprintf("gateway %s is ready", store.Name))
	return ctrl.Result{}, nil
}

// reconcileReady re-applies the owned objects, so a spec change (replicas,
// image, placement) converges without leaving Ready, and refreshes
// status.readyReplicas. design §6's own failure-table row for
// spec.replicas: "ordinary Deployment rolling scale-down... stays Ready."
func (r *ObjectStoreReconciler) reconcileReady(
	ctx context.Context, store *objectstorev1alpha1.ObjectStore,
) (ctrl.Result, error) {
	if err := r.applyOwnedObjects(ctx, store); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling the gateway's Deployment, Service, and certificate: %w", err)
	}

	_, readyReplicas, err := r.deploymentAvailable(ctx, store)
	if err != nil {
		return ctrl.Result{}, err
	}
	if readyReplicas == store.Status.ReadyReplicas {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.writeStatus(ctx, store, func(s *objectstorev1alpha1.ObjectStoreStatus) {
		s.ReadyReplicas = readyReplicas
	})
}

// applyOwnedObjects creates or updates the Deployment and the Service, and
// the cert-manager Certificate where that provider is selected (the
// OpenShift provider needs no object of its own: its annotation is already
// on the Service buildService returns).
func (r *ObjectStoreReconciler) applyOwnedObjects(ctx context.Context, store *objectstorev1alpha1.ObjectStore) error {
	if err := r.apply(ctx, buildDeployment(store), store); err != nil {
		return fmt.Errorf("the Deployment: %w", err)
	}
	if err := r.applyService(ctx, store); err != nil {
		return fmt.Errorf("the Service: %w", err)
	}
	if utils.IsCertManagerTLSProvider(store.Spec.TLSProvider) {
		if err := r.applyCertificate(ctx, store); err != nil {
			return fmt.Errorf("the certificate: %w", err)
		}
	}
	return nil
}

// apply creates desired or updates it in place, carrying forward the
// existing object's resourceVersion. Used for the Deployment, which has no
// server-assigned field that a blind overwrite would lose (the Service's own
// ClusterIP does, so it gets its own path below).
func (r *ObjectStoreReconciler) apply(
	ctx context.Context, desired *appsv1.Deployment, store *objectstorev1alpha1.ObjectStore,
) error {
	if err := controllerutil.SetControllerReference(store, desired, r.Scheme); err != nil {
		return err
	}
	var existing appsv1.Deployment
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	return r.Update(ctx, desired)
}

// applyService creates the Service or updates it in place, carrying forward
// the cluster-assigned ClusterIP, which a blind overwrite would otherwise
// clear and fail admission on.
func (r *ObjectStoreReconciler) applyService(ctx context.Context, store *objectstorev1alpha1.ObjectStore) error {
	desired := buildService(store)
	if err := controllerutil.SetControllerReference(store, desired, r.Scheme); err != nil {
		return err
	}
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	desired.Spec.ClusterIP = existing.Spec.ClusterIP
	return r.Update(ctx, desired)
}

// applyCertificate creates or updates the cert-manager Certificate that
// issues the Service's serving certificate, the same call
// StorageNodeWorkloadReconciler.reconcileCertificates already makes
// (internal/controllers/node/workload_controller.go) for its own Services.
func (r *ObjectStoreReconciler) applyCertificate(ctx context.Context, store *objectstorev1alpha1.ObjectStore) error {
	object := utils.BuildServiceServingCertificate(store.Namespace, store.Name, tlsSecretName(store.Name))
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, object, func() error {
		desired := utils.BuildServiceServingCertificate(store.Namespace, store.Name, tlsSecretName(store.Name))
		object.Object["spec"] = desired.Object["spec"]
		return controllerutil.SetControllerReference(store, object, r.Scheme)
	})
	return err
}

// deploymentAvailable reports whether the gateway Deployment's own
// DeploymentAvailable condition is True, and its current ready replica
// count, which the caller mirrors into status regardless.
func (r *ObjectStoreReconciler) deploymentAvailable(
	ctx context.Context, store *objectstorev1alpha1.ObjectStore,
) (available bool, readyReplicas int32, err error) {
	var deployment appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: store.Namespace, Name: store.Name}, &deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return false, 0, nil
		}
		return false, 0, err
	}
	for _, cond := range deployment.Status.Conditions {
		if cond.Type == appsv1.DeploymentAvailable {
			return cond.Status == corev1.ConditionTrue, deployment.Status.ReadyReplicas, nil
		}
	}
	return false, deployment.Status.ReadyReplicas, nil
}

// certificateReady reports whether the cert-manager Certificate's own Ready
// condition is True. The OpenShift provider creates no Certificate object at
// all (its annotation is self-sufficient, §5), so it is read as ready
// immediately: there is nothing this reconciler can observe about whether
// the service-ca-operator has acted yet, the same boundary §9's failure
// table draws around it.
func (r *ObjectStoreReconciler) certificateReady(
	ctx context.Context, store *objectstorev1alpha1.ObjectStore,
) (bool, error) {
	if !utils.IsCertManagerTLSProvider(store.Spec.TLSProvider) {
		return true, nil
	}

	var certificate unstructured.Unstructured
	certificate.SetGroupVersionKind(certificateGVK)
	err := r.Get(ctx, client.ObjectKey{Namespace: store.Namespace, Name: store.Name}, &certificate)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return unstructuredConditionTrue(&certificate, "Ready"), nil
}

// provisioningFailure names why Provisioning timed out: whichever of the two
// conditions is not yet satisfied, the Deployment's own or the
// certificate's. Called once, from Reconcile, when sm.TimeoutReached() is
// true.
func (r *ObjectStoreReconciler) provisioningFailure(
	ctx context.Context, store *objectstorev1alpha1.ObjectStore,
) (reason, message, conditionType string) {
	available, _, err := r.deploymentAvailable(ctx, store)
	if err == nil && !available {
		return "DeploymentNotAvailable",
			fmt.Sprintf("gateway Deployment %s did not become Available within the retry budget", store.Name),
			conditionDeploymentAvailable
	}
	return "CertificateNotReady",
		fmt.Sprintf("the Service's certificate for %s was not issued within the retry budget", store.Name),
		conditionCertificateReady
}

// toDegraded parks the ObjectStore and says why, in a condition and an
// event.
func (r *ObjectStoreReconciler) toDegraded(
	ctx context.Context,
	store *objectstorev1alpha1.ObjectStore,
	sm *statemachine.Machine[osPhase],
	reason, message, conditionType string,
) error {
	if err := sm.TransitionTo(ctx, osPhaseDegraded); err != nil {
		return fmt.Errorf("transition to Degraded: %w", err)
	}
	r.event(store, corev1.EventTypeWarning, reason, message)
	return r.writeStatus(ctx, store, func(s *objectstorev1alpha1.ObjectStoreStatus) {
		s.Phase = osPhaseDegraded
		s.Message = message
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: conditionType, Status: metav1.ConditionFalse, Reason: reason, Message: message,
		})
	})
}

// restore rebuilds the state machine from status, so a process restarted
// mid-Provisioning resumes the same deadline rather than granting a fresh
// one (design §6, I-06), and does not treat a phase already reached as a
// transition into the phase already happened.
func (r *ObjectStoreReconciler) restore(
	ctx context.Context, store *objectstorev1alpha1.ObjectStore,
) (*statemachine.Machine[osPhase], error) {
	snap := statemachine.Snapshot[osPhase]{State: store.Status.Phase}
	if store.Status.PhaseDeadline != nil {
		snap.Deadline = store.Status.PhaseDeadline.Time
	}
	return statemachine.NewFromSnapshot(ctx, objectStorePhases(), snap)
}

// objectStorePhases is the declared graph. An edge not written here is an
// error at the transition rather than a silent status write. Ready and
// Degraded both have no exits: Ready reconciles routine spec changes in
// place without leaving it (design §6), and Degraded is reached by refusing
// to act, the same shape S3Bucket's and NFSExport's own Degraded already
// have in this manager.
func objectStorePhases() statemachine.Config[osPhase] {
	return statemachine.Config[osPhase]{
		Initial: osPhasePending,
		States: map[osPhase]statemachine.StateDef[osPhase]{
			osPhasePending: {To: []osPhase{osPhaseProvisioning}},
			osPhaseProvisioning: {
				To: []osPhase{osPhaseReady, osPhaseDegraded},
				OnEnter: func(context.Context, osPhase, osPhase) (time.Duration, error) {
					return objectStoreProvisioningDeadline, nil
				},
			},
			osPhaseReady:    {To: []osPhase{}},
			osPhaseDegraded: {},
		},
	}
}

// setObjectStoreDeadline copies the machine's bound into status, to outlive
// the process.
func setObjectStoreDeadline(s *objectstorev1alpha1.ObjectStoreStatus, sm *statemachine.Machine[osPhase]) {
	snap := sm.Snapshot()
	if snap.Deadline.IsZero() {
		s.PhaseDeadline = nil
		return
	}
	s.PhaseDeadline = &metav1.Time{Time: snap.Deadline}
}

// writeStatus applies mutate to a freshly read copy, retrying on conflict.
// Re-reading is the point: a blind overwrite reverts a concurrent writer.
func (r *ObjectStoreReconciler) writeStatus(
	ctx context.Context,
	store *objectstorev1alpha1.ObjectStore,
	mutate func(*objectstorev1alpha1.ObjectStoreStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh objectstorev1alpha1.ObjectStore
		if err := r.Get(ctx, client.ObjectKeyFromObject(store), &fresh); err != nil {
			return err
		}
		mutate(&fresh.Status)
		fresh.Status.ObservedGeneration = fresh.Generation
		if err := r.Status().Update(ctx, &fresh); err != nil {
			return err
		}
		fresh.Status.DeepCopyInto(&store.Status)
		return nil
	})
}

func (r *ObjectStoreReconciler) event(store *objectstorev1alpha1.ObjectStore, kind, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(store, nil, kind, reason, reason, "%s", message)
	}
}

func objectStoreControllerOptions() controller.Options {
	return controller.Options{MaxConcurrentReconciles: objectStoreConcurrency}
}

// certificateGVK is what the unstructured Certificate this reconciler reads
// and writes carries, the same apiVersion/kind
// utils.BuildServiceServingCertificate builds.
var certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

// unstructuredConditionTrue reports whether obj's status.conditions carries
// condType with status "True," the shape every cert-manager (and
// Kubernetes-API-convention) condition list takes. A condition list that is
// missing, malformed, or simply does not name condType reads as false rather
// than erroring: a Certificate with no status yet is indistinguishable from
// one that is not Ready, which is exactly what "not Ready yet" means during
// Provisioning.
func unstructuredConditionTrue(obj *unstructured.Unstructured, condType string) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conditions {
		condition, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if condition["type"] == condType {
			status, _ := condition["status"].(string)
			return status == "True"
		}
	}
	return false
}

// SetupWithManager wires the controller. Owns the Deployment and the
// Service it creates, so a manual edit or delete of either is noticed and
// repaired on the ObjectStore's own reconcile. The certificate is
// unstructured and owned the same way the Deployment and the Service are
// (an owner reference, set in applyCertificate), but is not watched: this
// manager has no cert-manager scheme registered to decode it as a typed
// object, and nothing here needs to react to a certificate's own status
// change faster than the next Provisioning poll already does.
func (r *ObjectStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&objectstorev1alpha1.ObjectStore{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		WithOptions(objectStoreControllerOptions()).
		Complete(r)
}
