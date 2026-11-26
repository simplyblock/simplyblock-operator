// ObjectStoreReconciler drives one S3 gateway's workload from creation to
// Ready: the Deployment, the ClusterIP Service, that Service's serving
// certificate (design-objectstore.md), and the gateway's ServiceAccount,
// Role, and RoleBinding. Every owned object is garbage-collected by Kubernetes
// on delete. A finalizer holds the delete while S3Buckets still reference the
// ObjectStore, so its gateway keeps serving them until they are gone (§4.2).

package objectstore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// osPhase is an alias so the graph below reads as a table. Named osPhase
// rather than phase: S3BucketReconciler's own phase/phasePending/... already
// claim those names in this package (s3bucket_controller.go), one kind each.
type osPhase = simplyblockv1alpha2.ObjectStorePhase

const (
	osPhasePending      = simplyblockv1alpha2.ObjectStorePhasePending
	osPhaseProvisioning = simplyblockv1alpha2.ObjectStorePhaseProvisioning
	osPhaseReady        = simplyblockv1alpha2.ObjectStorePhaseReady
	osPhaseDegraded     = simplyblockv1alpha2.ObjectStorePhaseDegraded
)

// Condition types this reconciler sets, design §4.2's own names.
const (
	conditionDeploymentAvailable = "DeploymentAvailable"
	conditionCertificateReady    = "CertificateReady"
	// BucketsRemaining is True while a requested delete is held because
	// S3Buckets still reference the ObjectStore (design §4.2).
	conditionBucketsRemaining = "BucketsRemaining"
)

// objectStoreBucketsFinalizer holds an ObjectStore's delete until no S3Bucket
// references it.
const objectStoreBucketsFinalizer = "storage.simplyblock.io/objectstore-buckets"

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

// objectStoreFieldOwner is the field manager the Deployment and Service
// applies below write under (the same name simplyblockdriver_controller.go
// and controlplane/apply.go use for their own server-side applies). A
// server-side apply only claims the fields actually present on the object
// this reconciler builds, a sparse one by design (design-objectstore.md
// §4.1's passthrough fields plus the few this reconciler owns outright), so
// resubmitting it when nothing changed is a true no-op: any field Kubernetes
// itself defaults (DNS policy, termination-message settings, the rolling
// update strategy, and so on) that this reconciler never sets is left
// exactly as it was, rather than manufacturing a diff on every pass the way
// a blind PUT of that same sparse object would.
const objectStoreFieldOwner = client.FieldOwner("simplyblock-operator")

// ObjectStoreReconciler reconciles ObjectStore objects.
type ObjectStoreReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=objectstores,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=objectstores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=objectstores/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

func (r *ObjectStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("objectstore", req.NamespacedName)

	var store simplyblockv1alpha2.ObjectStore
	if err := r.Get(ctx, req.NamespacedName, &store); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !store.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDelete(ctx, &store)
	}
	// Update refreshes store in place, so this pass continues on the copy
	// the finalizer write returned.
	if controllerutil.AddFinalizer(&store, objectStoreBucketsFinalizer) {
		if err := r.Update(ctx, &store); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Degraded is reached by refusing to act, and retrying the refusal on a
	// timer would bury the event that explains it. It is left only when the
	// object it was waiting on changes (design §6).
	if store.Status.Phase == osPhaseDegraded {
		return r.reconcileDegraded(ctx, &store)
	}

	sm, err := r.restore(ctx, &store)
	if err != nil {
		r.event(&store, corev1.EventTypeWarning, "PhaseUnrecognized",
			fmt.Sprintf("cannot restore phase %q", store.Status.Phase))
		return ctrl.Result{}, err
	}
	defer sm.Close()

	if sm.TimeoutReached() {
		reason, message, conditionType := r.provisioningFailure(ctx, &store)
		return ctrl.Result{}, r.toDegraded(ctx, &store, sm, reason, message, conditionType)
	}

	switch sm.CurrentState() {
	case osPhasePending:
		return r.reconcilePending(ctx, &store, sm)
	case osPhaseProvisioning:
		return r.reconcileProvisioning(ctx, &store, sm, logger)
	case osPhaseReady:
		return r.reconcileReady(ctx, &store, sm)
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
	ctx context.Context, store *simplyblockv1alpha2.ObjectStore, sm *statemachine.Machine[osPhase],
) (ctrl.Result, error) {
	if _, err := r.applyOwnedObjects(ctx, store); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating the gateway's Deployment, Service, and certificate: %w", err)
	}

	if err := sm.TransitionTo(ctx, osPhaseProvisioning); err != nil {
		return ctrl.Result{}, fmt.Errorf("transition to Provisioning: %w", err)
	}
	if err := r.writeStatus(ctx, store, func(s *simplyblockv1alpha2.ObjectStoreStatus) {
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
	ctx context.Context, store *simplyblockv1alpha2.ObjectStore, sm *statemachine.Machine[osPhase], logger logr,
) (ctrl.Result, error) {
	if _, err := r.applyOwnedObjects(ctx, store); err != nil {
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
	if err := r.writeStatus(ctx, store, func(s *simplyblockv1alpha2.ObjectStoreStatus) {
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
//
// A spec.tlsProvider change is the exception: once the previous issuer's
// state is retired, the store returns to Provisioning and waits there for
// the new issuer's certificate.
func (r *ObjectStoreReconciler) reconcileReady(
	ctx context.Context, store *simplyblockv1alpha2.ObjectStore, sm *statemachine.Machine[osPhase],
) (ctrl.Result, error) {
	retired, err := r.applyOwnedObjects(ctx, store)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling the gateway's Deployment, Service, and certificate: %w", err)
	}
	if retired {
		return r.reissue(ctx, store, sm)
	}

	_, readyReplicas, err := r.deploymentAvailable(ctx, store)
	if err != nil {
		return ctrl.Result{}, err
	}
	if readyReplicas == store.Status.ReadyReplicas {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.writeStatus(ctx, store, func(s *simplyblockv1alpha2.ObjectStoreStatus) {
		s.ReadyReplicas = readyReplicas
	})
}

// reissue returns a Ready store to Provisioning after a spec.tlsProvider
// change, with CertificateReady False until the new issuer's certificate is
// in place.
func (r *ObjectStoreReconciler) reissue(
	ctx context.Context, store *simplyblockv1alpha2.ObjectStore, sm *statemachine.Machine[osPhase],
) (ctrl.Result, error) {
	if err := sm.TransitionTo(ctx, osPhaseProvisioning); err != nil {
		return ctrl.Result{}, fmt.Errorf("transition to Provisioning: %w", err)
	}
	message := fmt.Sprintf("spec.tlsProvider is now %s: the previous issuer's certificate was removed, "+
		"waiting for the new one", store.Spec.TLSProvider)
	r.event(store, corev1.EventTypeNormal, "TLSProviderChanged", message)
	if err := r.writeStatus(ctx, store, func(s *simplyblockv1alpha2.ObjectStoreStatus) {
		s.Phase = osPhaseProvisioning
		setObjectStoreDeadline(s, sm)
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: conditionCertificateReady, Status: metav1.ConditionFalse, Reason: "TLSProviderChanged", Message: message,
		})
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: objectStoreFreshCopyRequeue}, nil
}

// applyOwnedObjects creates or updates the gateway's ServiceAccount, Role,
// and RoleBinding, the Deployment that runs as that ServiceAccount, the
// Service, and the cert-manager Certificate where that provider is selected
// (the OpenShift provider needs no object of its own: its annotation is
// already on the Service buildService returns). It reports whether it
// retired the state of a previously selected TLS provider.
func (r *ObjectStoreReconciler) applyOwnedObjects(
	ctx context.Context, store *simplyblockv1alpha2.ObjectStore,
) (retired bool, err error) {
	for _, owned := range []struct {
		what string
		obj  client.Object
	}{
		{"the ServiceAccount", buildServiceAccount(store)},
		{"the Role", buildRole(store)},
		{"the RoleBinding", buildRoleBinding(store)},
	} {
		if err := r.apply(ctx, owned.obj, store); err != nil {
			return false, fmt.Errorf("%s: %w", owned.what, err)
		}
	}
	if err := r.apply(ctx, buildDeployment(store), store); err != nil {
		return false, fmt.Errorf("the Deployment: %w", err)
	}
	if err := r.applyService(ctx, store); err != nil {
		return false, fmt.Errorf("the Service: %w", err)
	}
	// After the Service, which drops or adds the service CA's annotation, and
	// before the Certificate, so the new issuer writes a fresh Secret rather
	// than one the previous issuer still holds.
	if retired, err = r.retirePreviousIssuer(ctx, store); err != nil {
		return false, fmt.Errorf("retiring the previous TLS provider: %w", err)
	}
	if utils.IsCertManagerTLSProvider(store.Spec.TLSProvider) {
		if err := r.applyCertificate(ctx, store); err != nil {
			return false, fmt.Errorf("the certificate: %w", err)
		}
	}
	return retired, nil
}

// retirePreviousIssuer removes what the TLS provider not selected left
// behind, so only one issuer writes the serving Secret: with OpenShift
// selected, the store's cert-manager Certificate and a Secret cert-manager
// wrote; with cert-manager selected, a Secret the service CA wrote. A Secret
// neither issuer's annotation names this store on is never deleted. It
// reports whether anything was deleted.
func (r *ObjectStoreReconciler) retirePreviousIssuer(
	ctx context.Context, store *simplyblockv1alpha2.ObjectStore,
) (bool, error) {
	retired := false
	staleAnnotation := serviceCAOriginatingServiceAnnotation
	if !utils.IsCertManagerTLSProvider(store.Spec.TLSProvider) {
		staleAnnotation = certManagerCertificateNameAnnotation
		var certificate unstructured.Unstructured
		certificate.SetGroupVersionKind(certificateGVK)
		certificate.SetNamespace(store.Namespace)
		certificate.SetName(store.Name)
		deleted, err := r.deleteIfPresent(ctx, &certificate)
		if err != nil {
			return false, fmt.Errorf("the cert-manager Certificate: %w", err)
		}
		retired = deleted
	}

	var secret corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: store.Namespace, Name: tlsSecretName(store.Name)}, &secret)
	if apierrors.IsNotFound(err) {
		return retired, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the serving Secret: %w", err)
	}
	if secret.Annotations[staleAnnotation] != store.Name {
		return retired, nil
	}
	deleted, err := r.deleteIfPresent(ctx, &secret)
	if err != nil {
		return false, fmt.Errorf("the serving Secret: %w", err)
	}
	return retired || deleted, nil
}

// deleteIfPresent deletes obj, reporting whether it existed. A cluster
// without cert-manager's CRDs has no Certificate to delete.
func (r *ObjectStoreReconciler) deleteIfPresent(ctx context.Context, obj client.Object) (bool, error) {
	err := r.Delete(ctx, obj)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return false, nil
	}
	return err == nil, err
}

// apply writes an owned object (the Deployment, and the gateway's
// ServiceAccount, Role, and RoleBinding) as a server-side apply under
// objectStoreFieldOwner, rather than a Get-then-blind-Update of a freshly
// built object.
//
// reconcileProvisioning calls this on every pass while waiting for the
// Deployment to report Available. A blind Update there, even one that
// carries forward the existing resourceVersion, resubmits a sparse object:
// buildDeployment sets only the fields design-objectstore.md §4.1 gives this
// reconciler an opinion about, never the fields Kubernetes itself defaults
// (the rolling-update strategy, DNS/restart policy, each container's
// termination-message settings, and more). Overwriting the whole object with
// that sparse one manufactures a diff against the fully defaulted object
// already stored on every single pass, which on a live cluster was observed
// bumping the Deployment's generation past 2900 within seven minutes, fast
// enough to starve the real Deployment controller of a quiet moment to
// report Available before this reconciler's own provisioning deadline ran
// out. A server-side apply only ever claims the fields actually present on
// the object serialized below (Go's encoding/json omits a zero-valued
// optional field, and so does the unstructured conversion apply uses), so a
// field this reconciler does not set is never touched, and reapplying the
// same sparse object is the true no-op idempotency requires.
func (r *ObjectStoreReconciler) apply(
	ctx context.Context, desired client.Object, store *simplyblockv1alpha2.ObjectStore,
) error {
	if err := controllerutil.SetControllerReference(store, desired, r.Scheme); err != nil {
		return err
	}
	cfg, err := r.applyConfiguration(desired)
	if err != nil {
		return err
	}
	return r.Apply(ctx, cfg, objectStoreFieldOwner, client.ForceOwnership)
}

// applyService writes the Service the same way apply writes the Deployment,
// and for the same reason: a server-side apply never touches a field this
// reconciler does not set, which is what lets it skip carrying the
// cluster-assigned ClusterIP forward by hand the way a blind Update would
// have needed to.
func (r *ObjectStoreReconciler) applyService(ctx context.Context, store *simplyblockv1alpha2.ObjectStore) error {
	desired := buildService(store)
	if err := controllerutil.SetControllerReference(store, desired, r.Scheme); err != nil {
		return err
	}
	cfg, err := r.applyConfiguration(desired)
	if err != nil {
		return err
	}
	return r.Apply(ctx, cfg, objectStoreFieldOwner, client.ForceOwnership)
}

// applyConfiguration turns a built object into the shape a server-side apply
// takes, the same conversion simplyblockdriver_controller.go's own
// applyConfiguration performs (not shared: each is a few lines, scoped to
// one small package, and the two reconcilers have no other reason to depend
// on each other).
//
// The apiVersion and kind have to be on the wire for an apply, and a typed
// object built in Go carries an empty TypeMeta, so the kind is resolved from
// the scheme rather than written by hand at each call site.
func (r *ObjectStoreReconciler) applyConfiguration(obj client.Object) (runtime.ApplyConfiguration, error) {
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
	// A built object has no status, and an apply that carries an empty one
	// claims ownership of a field this reconciler does not set.
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	return client.ApplyConfigurationFromUnstructured(u), nil
}

// applyCertificate creates or updates the cert-manager Certificate that
// issues the Service's serving certificate, the same call
// StorageNodeWorkloadReconciler.reconcileCertificates already makes
// (internal/controllers/node/workload_controller.go) for its own Services.
func (r *ObjectStoreReconciler) applyCertificate(ctx context.Context, store *simplyblockv1alpha2.ObjectStore) error {
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
	ctx context.Context, store *simplyblockv1alpha2.ObjectStore,
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
// all (its annotation is self-sufficient, §5), so for it the serving Secret
// is what is read: ready once it exists and cert-manager did not write it.
// After a switch from cert-manager, that is the service CA's Secret.
func (r *ObjectStoreReconciler) certificateReady(
	ctx context.Context, store *simplyblockv1alpha2.ObjectStore,
) (bool, error) {
	if !utils.IsCertManagerTLSProvider(store.Spec.TLSProvider) {
		var secret corev1.Secret
		err := r.Get(ctx, client.ObjectKey{Namespace: store.Namespace, Name: tlsSecretName(store.Name)}, &secret)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		_, issuedByCertManager := secret.Annotations[certManagerCertificateNameAnnotation]
		return !issuedByCertManager, nil
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
	ctx context.Context, store *simplyblockv1alpha2.ObjectStore,
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
	store *simplyblockv1alpha2.ObjectStore,
	sm *statemachine.Machine[osPhase],
	reason, message, conditionType string,
) error {
	if err := sm.TransitionTo(ctx, osPhaseDegraded); err != nil {
		return fmt.Errorf("transition to Degraded: %w", err)
	}
	r.event(store, corev1.EventTypeWarning, reason, message)
	return r.writeStatus(ctx, store, func(s *simplyblockv1alpha2.ObjectStoreStatus) {
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
	ctx context.Context, store *simplyblockv1alpha2.ObjectStore,
) (*statemachine.Machine[osPhase], error) {
	snap := statemachine.Snapshot[osPhase]{State: store.Status.Phase}
	if store.Status.PhaseDeadline != nil {
		snap.Deadline = store.Status.PhaseDeadline.Time
	}
	return statemachine.NewFromSnapshot(ctx, objectStorePhases(), snap)
}

// objectStorePhases is the declared graph. An edge not written here is an
// error at the transition rather than a silent status write. Ready
// reconciles routine spec changes in place without leaving it (design §6),
// and returns to Provisioning only after a spec.tlsProvider change, to wait
// for the new issuer's certificate (reissue). Degraded returns to Provisioning, with a fresh deadline, once
// the object it was waiting on changes (reconcileDegraded).
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
			osPhaseReady:    {To: []osPhase{osPhaseProvisioning}},
			osPhaseDegraded: {To: []osPhase{osPhaseProvisioning}},
		},
	}
}

// setObjectStoreDeadline copies the machine's bound into status, to outlive
// the process.
func setObjectStoreDeadline(s *simplyblockv1alpha2.ObjectStoreStatus, sm *statemachine.Machine[osPhase]) {
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
	store *simplyblockv1alpha2.ObjectStore,
	mutate func(*simplyblockv1alpha2.ObjectStoreStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.ObjectStore
		if err := r.Get(ctx, client.ObjectKeyFromObject(store), &fresh); err != nil {
			return err
		}
		mutate(&fresh.Status)
		// The generation this pass read, not the fresh one: a spec edit that
		// landed since has not been acted on, and a Degraded store leaves on an
		// unobserved generation. One a later pass recorded is kept.
		fresh.Status.ObservedGeneration = max(fresh.Status.ObservedGeneration, store.Generation)
		if err := r.Status().Update(ctx, &fresh); err != nil {
			return err
		}
		fresh.Status.DeepCopyInto(&store.Status)
		return nil
	})
}

func (r *ObjectStoreReconciler) event(store *simplyblockv1alpha2.ObjectStore, kind, reason, message string) {
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

// SetupWithManager wires the controller. Owns the Deployment, the Service,
// and the gateway's ServiceAccount, Role, and RoleBinding it creates, so a manual edit or delete of either is noticed and
// repaired on the ObjectStore's own reconcile. Watches Secrets and ConfigMaps
// and enqueues the ObjectStores whose envFrom names them, which is what lets
// a Degraded ObjectStore notice a missing reference being created. Watches
// S3Buckets and enqueues the ObjectStore each names, so a held delete
// completes when the last bucket is gone. The certificate is
// unstructured and owned the same way the Deployment and the Service are
// (an owner reference, set in applyCertificate), but is not watched: this
// manager has no cert-manager scheme registered to decode it as a typed
// object, and nothing here needs to react to a certificate's own status
// change faster than the next Provisioning poll already does.
func (r *ObjectStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.ObjectStore{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(&simplyblockv1alpha2.S3Bucket{}, handler.EnqueueRequestsFromMapFunc(storeOfBucket)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.storesReferencingEnvFrom)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.storesReferencingEnvFrom)).
		WithOptions(objectStoreControllerOptions()).
		Complete(r)
}

// objectStoreDegradedCertificatePoll is how often an ObjectStore Degraded on
// its certificate re-reads it. The Certificate is unstructured and not
// watched (SetupWithManager), so a poll is what notices it turning Ready.
const objectStoreDegradedCertificatePoll = time.Minute

// reconcileDegraded returns the ObjectStore to Provisioning when the cause it
// degraded on may be gone: its spec changed since it degraded, which may be
// the fix (a corrected image, for one), a Secret or ConfigMap named in
// spec.envFrom was created or changed after it degraded, or, when it degraded
// on its certificate, the Certificate now reports Ready. Otherwise, it stays
// Degraded, so the event that explains the refusal is not buried under
// retries.
func (r *ObjectStoreReconciler) reconcileDegraded(
	ctx context.Context, store *simplyblockv1alpha2.ObjectStore,
) (ctrl.Result, error) {
	since, onCertificate := degradedSince(store)
	recovered := store.Generation > store.Status.ObservedGeneration
	var err error
	if !recovered {
		if recovered, err = r.envFromChangedSince(ctx, store, since); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !recovered && onCertificate {
		if recovered, err = r.certificateReady(ctx, store); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !recovered {
		if onCertificate {
			return ctrl.Result{RequeueAfter: objectStoreDegradedCertificatePoll}, nil
		}
		return ctrl.Result{}, nil
	}

	sm, err := r.restore(ctx, store)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer sm.Close()
	if err := sm.TransitionTo(ctx, osPhaseProvisioning); err != nil {
		return ctrl.Result{}, fmt.Errorf("transition to Provisioning: %w", err)
	}
	r.event(store, corev1.EventTypeNormal, "Recovering",
		"the object this ObjectStore was waiting on changed, provisioning again")
	if err := r.writeStatus(ctx, store, func(s *simplyblockv1alpha2.ObjectStoreStatus) {
		s.Phase = osPhaseProvisioning
		s.Message = ""
		setObjectStoreDeadline(s, sm)
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: objectStoreFreshCopyRequeue}, nil
}

// degradedSince is when the ObjectStore degraded, read from the condition
// toDegraded set False, and whether that condition is the certificate's.
func degradedSince(store *simplyblockv1alpha2.ObjectStore) (since time.Time, onCertificate bool) {
	for _, condType := range []string{conditionDeploymentAvailable, conditionCertificateReady} {
		cond := meta.FindStatusCondition(store.Status.Conditions, condType)
		if cond != nil && cond.Status == metav1.ConditionFalse && cond.LastTransitionTime.After(since) {
			since, onCertificate = cond.LastTransitionTime.Time, condType == conditionCertificateReady
		}
	}
	return since, onCertificate
}

// envFromChangedSince reports whether a Secret or ConfigMap named in
// spec.envFrom was created or last written after since. A reference that
// still does not exist has not changed.
func (r *ObjectStoreReconciler) envFromChangedSince(
	ctx context.Context, store *simplyblockv1alpha2.ObjectStore, since time.Time,
) (bool, error) {
	for _, ref := range store.Spec.EnvFrom {
		var obj client.Object
		switch {
		case ref.SecretRef != nil:
			obj = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ref.SecretRef.Name}}
		case ref.ConfigMapRef != nil:
			obj = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ref.ConfigMapRef.Name}}
		default:
			continue
		}
		err := r.Get(ctx, client.ObjectKey{Namespace: store.Namespace, Name: obj.GetName()}, obj)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if lastWritten(obj).After(since) {
			return true, nil
		}
	}
	return false, nil
}

// lastWritten is the later of obj's creation and its newest managed-fields
// write.
func lastWritten(obj client.Object) time.Time {
	latest := obj.GetCreationTimestamp().Time
	for _, entry := range obj.GetManagedFields() {
		if entry.Time != nil && entry.Time.After(latest) {
			latest = entry.Time.Time
		}
	}
	return latest
}

// storesReferencingEnvFrom maps a Secret or ConfigMap to the ObjectStores in
// its namespace whose spec.envFrom names it.
func (r *ObjectStoreReconciler) storesReferencingEnvFrom(ctx context.Context, obj client.Object) []ctrl.Request {
	var stores simplyblockv1alpha2.ObjectStoreList
	if err := r.List(ctx, &stores, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing ObjectStores for an envFrom reference", "name", obj.GetName())
		return nil
	}
	_, isSecret := obj.(*corev1.Secret)
	var requests []ctrl.Request
	for i := range stores.Items {
		for _, ref := range stores.Items[i].Spec.EnvFrom {
			if isSecret && ref.SecretRef != nil && ref.SecretRef.Name == obj.GetName() ||
				!isSecret && ref.ConfigMapRef != nil && ref.ConfigMapRef.Name == obj.GetName() {
				requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&stores.Items[i])})
				break
			}
		}
	}
	return requests
}

// reconcileDelete holds the delete while S3Buckets reference the ObjectStore,
// saying so in BucketsRemaining, and releases the finalizer once none do.
// The phase is left as it is: the gateway keeps serving the remaining
// buckets until then.
func (r *ObjectStoreReconciler) reconcileDelete(ctx context.Context, store *simplyblockv1alpha2.ObjectStore) error {
	if !controllerutil.ContainsFinalizer(store, objectStoreBucketsFinalizer) {
		return nil
	}
	var buckets simplyblockv1alpha2.S3BucketList
	if err := r.List(ctx, &buckets, client.InNamespace(store.Namespace)); err != nil {
		return fmt.Errorf("listing the S3Buckets that may reference %s: %w", store.Name, err)
	}
	var remaining []string
	for i := range buckets.Items {
		if buckets.Items[i].Spec.ObjectStoreRef == store.Name {
			remaining = append(remaining, buckets.Items[i].Name)
		}
	}
	if len(remaining) > 0 {
		sort.Strings(remaining)
		message := fmt.Sprintf("deletion waits for S3Buckets still referencing this ObjectStore: %s", strings.Join(remaining, ", "))
		if cond := meta.FindStatusCondition(store.Status.Conditions, conditionBucketsRemaining); cond == nil || cond.Message != message {
			r.event(store, corev1.EventTypeNormal, conditionBucketsRemaining, message)
		}
		return r.writeStatus(ctx, store, func(s *simplyblockv1alpha2.ObjectStoreStatus) {
			meta.SetStatusCondition(&s.Conditions, metav1.Condition{
				Type: conditionBucketsRemaining, Status: metav1.ConditionTrue, Reason: conditionBucketsRemaining, Message: message,
			})
		})
	}
	controllerutil.RemoveFinalizer(store, objectStoreBucketsFinalizer)
	return client.IgnoreNotFound(r.Update(ctx, store))
}

// storeOfBucket maps an S3Bucket to the ObjectStore it references.
func storeOfBucket(_ context.Context, obj client.Object) []ctrl.Request {
	bucket, ok := obj.(*simplyblockv1alpha2.S3Bucket)
	if !ok || bucket.Spec.ObjectStoreRef == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: bucket.Namespace, Name: bucket.Spec.ObjectStoreRef}}}
}
