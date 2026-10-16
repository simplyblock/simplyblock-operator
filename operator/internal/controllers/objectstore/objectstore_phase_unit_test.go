// The ObjectStore phase-transition table (design-objectstore.md §6), proved
// against a fake client: no cluster, one reconcile pass per assertion.
// Deployment.status and the cert-manager Certificate's status.conditions are
// fixture data here (envtest runs no kubelet and no cert-manager controller
// either, so this is not a simplification unit tests make and integration
// tests do not: I-02's own fixtures set the same fields by hand).

package objectstore

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func testStore(mutate ...func(*objectstorev1alpha1.ObjectStore)) *objectstorev1alpha1.ObjectStore {
	store := testObjectStoreSpec()
	for _, m := range mutate {
		m(store)
	}
	return store
}

// availableDeployment is the gateway Deployment design-objectstore.md's own
// Reconcile would have created, carrying whatever Available condition and
// ready-replica count a scenario needs.
func availableDeployment(store *objectstorev1alpha1.ObjectStore, available bool, readyReplicas int32) *appsv1.Deployment {
	status := corev1.ConditionFalse
	if available {
		status = corev1.ConditionTrue
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: store.Name, Namespace: store.Namespace},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: readyReplicas,
			Conditions:    []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: status}},
		},
	}
}

// readyCertificate is the cert-manager Certificate this reconciler reads
// back, carrying a Ready condition of the given status.
func readyCertificate(store *objectstorev1alpha1.ObjectStore, ready bool) *unstructured.Unstructured {
	status := "False"
	if ready {
		status = "True"
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(store.Name)
	cert.SetNamespace(store.Namespace)
	_ = unstructured.SetNestedSlice(cert.Object, []any{
		map[string]any{"type": "Ready", "status": status},
	}, "status", "conditions")
	return cert
}

func reconcileStore(t *testing.T, cli client.Client, store *objectstorev1alpha1.ObjectStore) objectstorev1alpha1.ObjectStore {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &ObjectStoreReconciler{Client: cli, Scheme: cli.Scheme()}
	if _, err := r.Reconcile(ctx, ctrlRequest(store)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got objectstorev1alpha1.ObjectStore
	if err := cli.Get(ctx, client.ObjectKeyFromObject(store), &got); err != nil {
		t.Fatalf("reading back the ObjectStore: %v", err)
	}
	return got
}

// U-01: a fresh ObjectStore, before anything has reconciled it, restores to
// Pending: no Deployment exists yet, and nothing about it says otherwise.
func TestFreshObjectStoreRestoresToPending(t *testing.T) {
	store := testStore()
	r := &ObjectStoreReconciler{Client: newTestClient(t, store), Scheme: newTestScheme(t)}

	sm, err := r.restore(context.Background(), store)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	defer sm.Close()
	if sm.CurrentState() != osPhasePending {
		t.Errorf("phase = %q, want Pending", sm.CurrentState())
	}
}

// U-02: the first reconcile creates the Deployment, the Service, and (the
// default provider, OpenShift) nothing else, and enters Provisioning since
// the fresh Deployment is not Available yet.
func TestFirstReconcileCreatesObjectsAndEntersProvisioning(t *testing.T) {
	store := testStore()
	cli := newTestClient(t, store)

	got := reconcileStore(t, cli, store)

	if got.Status.Phase != osPhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", got.Status.Phase)
	}
	var deployment appsv1.Deployment
	if err := cli.Get(context.Background(), client.ObjectKeyFromObject(store), &deployment); err != nil {
		t.Fatalf("no Deployment was created: %v", err)
	}
	var service corev1.Service
	if err := cli.Get(context.Background(), client.ObjectKeyFromObject(store), &service); err != nil {
		t.Fatalf("no Service was created: %v", err)
	}
}

// U-03: the Deployment reporting Available and the certificate reporting
// Ready together reach Ready.
func TestProvisioningReachesReadyOnceBothConditionsAreMet(t *testing.T) {
	store := testStore(func(o *objectstorev1alpha1.ObjectStore) {
		o.Status.Phase = osPhaseProvisioning
		o.Spec.TLSProvider = utils.TLSProviderCertManager
	})
	cli := newTestClient(t, store, availableDeployment(store, true, 1), readyCertificate(store, true))

	got := reconcileStore(t, cli, store)

	if got.Status.Phase != osPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.ReadyReplicas != 1 {
		t.Errorf("readyReplicas = %d, want 1", got.Status.ReadyReplicas)
	}
}

// U-04: the Deployment Available but the certificate not yet Ready stays
// Provisioning.
func TestProvisioningStaysWhileCertificateIsNotReady(t *testing.T) {
	store := testStore(func(o *objectstorev1alpha1.ObjectStore) {
		o.Status.Phase = osPhaseProvisioning
		o.Spec.TLSProvider = utils.TLSProviderCertManager
	})
	cli := newTestClient(t, store, availableDeployment(store, true, 1), readyCertificate(store, false))

	got := reconcileStore(t, cli, store)

	if got.Status.Phase != osPhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", got.Status.Phase)
	}
}

// U-05: Provisioning past its deadline with the Deployment never becoming
// Available degrades, naming DeploymentAvailable False.
func TestProvisioningDeadlinePassedWithNoAvailableDeploymentDegrades(t *testing.T) {
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	store := testStore(func(o *objectstorev1alpha1.ObjectStore) {
		o.Status.Phase = osPhaseProvisioning
		o.Status.PhaseDeadline = &past
	})
	cli := newTestClient(t, store, availableDeployment(store, false, 0))

	got := reconcileStore(t, cli, store)

	if got.Status.Phase != osPhaseDegraded {
		t.Errorf("phase = %q, want Degraded", got.Status.Phase)
	}
	cond := findCondition(got.Status.Conditions, conditionDeploymentAvailable)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("DeploymentAvailable condition = %+v, want status False", cond)
	}
}

// U-06: Provisioning past its deadline with the Deployment Available but no
// matching cert-manager Issuer (observed here as the Certificate never
// reporting Ready) degrades, naming CertificateReady False.
func TestProvisioningDeadlinePassedWithNoReadyCertificateDegrades(t *testing.T) {
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	store := testStore(func(o *objectstorev1alpha1.ObjectStore) {
		o.Status.Phase = osPhaseProvisioning
		o.Status.PhaseDeadline = &past
		o.Spec.TLSProvider = utils.TLSProviderCertManager
	})
	cli := newTestClient(t, store, availableDeployment(store, true, 1), readyCertificate(store, false))

	got := reconcileStore(t, cli, store)

	if got.Status.Phase != osPhaseDegraded {
		t.Errorf("phase = %q, want Degraded", got.Status.Phase)
	}
	cond := findCondition(got.Status.Conditions, conditionCertificateReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("CertificateReady condition = %+v, want status False", cond)
	}
}

// U-07: a Ready ObjectStore with spec.replicas changed reconciles the
// Deployment in place and never returns to Pending.
func TestReadyObjectStoreReconcilesReplicasInPlace(t *testing.T) {
	store := testStore(func(o *objectstorev1alpha1.ObjectStore) {
		o.Status.Phase = osPhaseReady
		o.Status.ReadyReplicas = 1
		three := int32(3)
		o.Spec.Replicas = &three
	})
	cli := newTestClient(t, store, availableDeployment(store, true, 1))

	got := reconcileStore(t, cli, store)

	if got.Status.Phase == osPhasePending {
		t.Fatal("phase returned to Pending")
	}
	if got.Status.Phase != osPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
	var deployment appsv1.Deployment
	if err := cli.Get(context.Background(), client.ObjectKeyFromObject(store), &deployment); err != nil {
		t.Fatalf("reading back the Deployment: %v", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 3 {
		t.Errorf("Deployment replicas = %v, want 3", deployment.Spec.Replicas)
	}
}

// U-08: a reconciler built fresh mid-Provisioning (standing in for an
// operator restart, since nothing here is held in memory between calls
// anyway) resumes from status rather than recreating the Deployment.
//
// Asserted through a Create interceptor rather than a field like UID or
// CreationTimestamp: a real apiserver refuses to let either move on an
// Update regardless of what the request body says, but the fake client
// enforces neither, so a wholesale Update that does not explicitly carry
// them forward (this reconciler's own apply(), matching
// StorageNodeWorkloadReconciler.apply's identical shape) silently resets
// both on this client and would make the assertion fail even on the correct
// path.
func TestRestartMidProvisioningDoesNotRecreateTheDeployment(t *testing.T) {
	store := testStore(func(o *objectstorev1alpha1.ObjectStore) {
		o.Status.Phase = osPhaseProvisioning
	})
	existing := availableDeployment(store, false, 0)

	created := false
	cli := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithStatusSubresource(&objectstorev1alpha1.ObjectStore{}).
		WithObjects(store, existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(
				ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption,
			) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					created = true
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	_ = reconcileStore(t, cli, store)

	if created {
		t.Error("the Deployment was created again rather than updated in place")
	}
}
