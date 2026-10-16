// ObjectStoreReconciler driven against a real apiserver, with cert-manager's
// CRDs installed (testdata/cert-manager-crds.yaml) and no kubelet or
// cert-manager controller running: Deployment.status and the Certificate's
// status.conditions are set by the test directly, the same simplification
// design-objectstore.md §11 states for this class.

package objectstore

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func testObjectStoreFixture(name string, mutate ...func(*objectstorev1alpha1.ObjectStore)) *objectstorev1alpha1.ObjectStore {
	store := &objectstorev1alpha1.ObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: objectstorev1alpha1.ObjectStoreSpec{
			Image: "docker.io/simplyblock/gateway:main",
			Port:  9000,
		},
	}
	for _, m := range mutate {
		m(store)
	}
	return store
}

// reconcileStoreUntil calls Reconcile in a loop against a real apiserver
// until cond holds or the budget runs out.
func reconcileStoreUntil(
	t *testing.T, r *ObjectStoreReconciler, key client.ObjectKey, cond func(*objectstorev1alpha1.ObjectStore) bool,
) *objectstorev1alpha1.ObjectStore {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := r.Reconcile(context.Background(), ctrlRequestKey(key)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		var got objectstorev1alpha1.ObjectStore
		if err := r.Get(context.Background(), key, &got); err != nil {
			t.Fatalf("reading the ObjectStore back: %v", err)
		}
		if cond(&got) {
			return &got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the ObjectStore to reach the expected state")
	return nil
}

// markDeploymentAvailable writes the gateway Deployment's status directly,
// the simplification envtest's lack of a kubelet requires.
func markDeploymentAvailable(t *testing.T, cli client.Client, key client.ObjectKey, available bool, readyReplicas int32) {
	t.Helper()
	var deployment appsv1.Deployment
	if err := cli.Get(context.Background(), key, &deployment); err != nil {
		t.Fatalf("reading the Deployment to mark it Available: %v", err)
	}
	status := corev1.ConditionFalse
	if available {
		status = corev1.ConditionTrue
	}
	deployment.Status.Replicas = readyReplicas
	deployment.Status.ReadyReplicas = readyReplicas
	deployment.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: status}}
	if err := cli.Status().Update(context.Background(), &deployment); err != nil {
		t.Fatalf("writing the Deployment's status: %v", err)
	}
}

// markCertificateReady writes the cert-manager Certificate's status
// directly, the simplification envtest's lack of a cert-manager controller
// requires.
func markCertificateReady(t *testing.T, cli client.Client, key client.ObjectKey, ready bool) {
	t.Helper()
	var cert unstructured.Unstructured
	cert.SetGroupVersionKind(certificateGVK)
	if err := cli.Get(context.Background(), key, &cert); err != nil {
		t.Fatalf("reading the Certificate to mark it Ready: %v", err)
	}
	status := "False"
	if ready {
		status = "True"
	}
	if err := unstructured.SetNestedSlice(cert.Object, []any{
		map[string]any{"type": "Ready", "status": status},
	}, "status", "conditions"); err != nil {
		t.Fatalf("setting the Certificate's conditions: %v", err)
	}
	if err := cli.Status().Update(context.Background(), &cert); err != nil {
		t.Fatalf("writing the Certificate's status: %v", err)
	}
}

// I-01: an ObjectStore created with the cert-manager provider gets a
// Deployment, a Service, and a Certificate, each owned via ownerReferences.
func TestObjectStoreCreatesItsOwnedObjects(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := testObjectStoreFixture("os-i01-store", func(o *objectstorev1alpha1.ObjectStore) {
		o.Spec.TLSProvider = utils.TLSProviderCertManager
	})
	if err := cli.Create(ctx, store); err != nil {
		t.Fatalf("creating the ObjectStore: %v", err)
	}

	r := &ObjectStoreReconciler{Client: cli, Scheme: cli.Scheme()}
	key := client.ObjectKeyFromObject(store)
	reconcileStoreUntil(t, r, key, func(o *objectstorev1alpha1.ObjectStore) bool {
		return o.Status.Phase == osPhaseProvisioning
	})

	for _, check := range []struct {
		name string
		obj  client.Object
	}{
		{"Deployment", &appsv1.Deployment{}},
		{"Service", &corev1.Service{}},
	} {
		if err := cli.Get(ctx, key, check.obj); err != nil {
			t.Fatalf("no %s was created: %v", check.name, err)
		}
		owners := check.obj.GetOwnerReferences()
		if len(owners) != 1 || owners[0].Name != store.Name || owners[0].Kind != "ObjectStore" {
			t.Errorf("%s owner references = %+v, want exactly one naming ObjectStore/%s", check.name, owners, store.Name)
		}
	}

	var cert unstructured.Unstructured
	cert.SetGroupVersionKind(certificateGVK)
	if err := cli.Get(ctx, key, &cert); err != nil {
		t.Fatalf("no Certificate was created: %v", err)
	}
	owners := cert.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Name != store.Name {
		t.Errorf("Certificate owner references = %+v, want exactly one naming ObjectStore/%s", owners, store.Name)
	}
}

// I-02: the Deployment and the Certificate faked as ready together bring
// the ObjectStore to Ready, with status.readyReplicas mirroring the
// Deployment.
func TestObjectStoreReachesReadyOnceDeploymentAndCertificateAreReady(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := testObjectStoreFixture("os-i02-store", func(o *objectstorev1alpha1.ObjectStore) {
		o.Spec.TLSProvider = utils.TLSProviderCertManager
	})
	if err := cli.Create(ctx, store); err != nil {
		t.Fatalf("creating the ObjectStore: %v", err)
	}

	recorder := events.NewFakeRecorder(16)
	r := &ObjectStoreReconciler{Client: cli, Scheme: cli.Scheme(), Recorder: recorder}
	key := client.ObjectKeyFromObject(store)
	reconcileStoreUntil(t, r, key, func(o *objectstorev1alpha1.ObjectStore) bool {
		return o.Status.Phase == osPhaseProvisioning
	})

	markDeploymentAvailable(t, cli, key, true, 2)
	markCertificateReady(t, cli, key, true)

	got := reconcileStoreUntil(t, r, key, func(o *objectstorev1alpha1.ObjectStore) bool {
		return o.Status.Phase == osPhaseReady
	})
	if got.Status.ReadyReplicas != 2 {
		t.Errorf("readyReplicas = %d, want 2", got.Status.ReadyReplicas)
	}
	expectEvent(t, recorder, "ObjectStoreReady")
}

// I-03: spec.replicas updated on an existing (Ready) ObjectStore updates the
// Deployment's own replica count, without touching the Service.
func TestObjectStoreReplicaChangeUpdatesTheDeploymentInPlace(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := testObjectStoreFixture("os-i03-store")
	if err := cli.Create(ctx, store); err != nil {
		t.Fatalf("creating the ObjectStore: %v", err)
	}

	r := &ObjectStoreReconciler{Client: cli, Scheme: cli.Scheme()}
	key := client.ObjectKeyFromObject(store)
	reconcileStoreUntil(t, r, key, func(o *objectstorev1alpha1.ObjectStore) bool {
		return o.Status.Phase == osPhaseProvisioning
	})
	markDeploymentAvailable(t, cli, key, true, 1)
	got := reconcileStoreUntil(t, r, key, func(o *objectstorev1alpha1.ObjectStore) bool {
		return o.Status.Phase == osPhaseReady
	})

	var service corev1.Service
	if err := cli.Get(ctx, key, &service); err != nil {
		t.Fatalf("reading the Service: %v", err)
	}
	serviceVersionBefore := service.ResourceVersion

	three := int32(3)
	got.Spec.Replicas = &three
	if err := cli.Update(ctx, got); err != nil {
		t.Fatalf("updating spec.replicas: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrlRequestKey(key)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var deployment appsv1.Deployment
	if err := cli.Get(ctx, key, &deployment); err != nil {
		t.Fatalf("reading the Deployment: %v", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 3 {
		t.Errorf("Deployment replicas = %v, want 3", deployment.Spec.Replicas)
	}

	if err := cli.Get(ctx, key, &service); err != nil {
		t.Fatalf("reading the Service: %v", err)
	}
	if service.ResourceVersion != serviceVersionBefore {
		t.Errorf("Service resourceVersion changed from %s to %s: it should not have been touched",
			serviceVersionBefore, service.ResourceVersion)
	}
}

// I-04: deleting a Ready ObjectStore completes immediately, since it has no
// finalizer of its own (design §4.2: owned objects are garbage-collected by
// Kubernetes, not this reconciler). Whether that garbage collection itself
// actually runs is not exercised here: envtest has no garbage-collector
// controller, only a real cluster does (E-01..E-03's own scope).
func TestObjectStoreDeletionIsNotBlockedByAFinalizer(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := testObjectStoreFixture("os-i04-store")
	if err := cli.Create(ctx, store); err != nil {
		t.Fatalf("creating the ObjectStore: %v", err)
	}
	r := &ObjectStoreReconciler{Client: cli, Scheme: cli.Scheme()}
	key := client.ObjectKeyFromObject(store)
	reconcileStoreUntil(t, r, key, func(o *objectstorev1alpha1.ObjectStore) bool {
		return o.Status.Phase == osPhaseProvisioning
	})

	if err := cli.Delete(ctx, store); err != nil {
		t.Fatalf("deleting the ObjectStore: %v", err)
	}

	var got objectstorev1alpha1.ObjectStore
	err := cli.Get(ctx, key, &got)
	if !apierrorsIsNotFound(err) {
		t.Errorf("Get after delete = %v, want NotFound (no finalizer should hold it back)", err)
	}
}

// I-06: a reconciler built fresh mid-Provisioning (standing in for an
// operator restart, since nothing here is held in memory between calls
// anyway) resumes from status rather than recreating the Deployment.
func TestObjectStoreResumesMidProvisioningWithoutRecreatingTheDeployment(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := testObjectStoreFixture("os-i06-store")
	if err := cli.Create(ctx, store); err != nil {
		t.Fatalf("creating the ObjectStore: %v", err)
	}
	key := client.ObjectKeyFromObject(store)
	first := &ObjectStoreReconciler{Client: cli, Scheme: cli.Scheme()}
	reconcileStoreUntil(t, first, key, func(o *objectstorev1alpha1.ObjectStore) bool {
		return o.Status.Phase == osPhaseProvisioning
	})

	var before appsv1.Deployment
	if err := cli.Get(ctx, key, &before); err != nil {
		t.Fatalf("reading the Deployment: %v", err)
	}

	// A freshly built reconciler, standing in for the process restarting:
	// nothing carried in memory, the way the real one is scoped per
	// manager.Start anyway.
	second := &ObjectStoreReconciler{Client: cli, Scheme: cli.Scheme()}
	if _, err := second.Reconcile(ctx, ctrlRequestKey(key)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var after appsv1.Deployment
	if err := cli.Get(ctx, key, &after); err != nil {
		t.Fatalf("reading the Deployment back: %v", err)
	}
	if after.UID != before.UID {
		t.Errorf("Deployment UID changed from %s to %s: it was recreated rather than updated", before.UID, after.UID)
	}
}

// Regression for a self-triggering reconcile storm: apply() used to call
// Update on the Deployment unconditionally on every Provisioning-phase
// reconcile. Since this controller also watches Deployments, each Update
// re-triggered its own reconcile far faster than the 1-second RequeueAfter
// backoff intends, bumping the Deployment's generation continuously and
// starving the real Deployment controller of a quiet moment to report
// Available — observed live as generation 2998 within seven minutes.
func TestObjectStoreProvisioningReconcileIsANoOpWhenNothingChanged(t *testing.T) {
	cli := apiServer(t)
	ctx := context.Background()

	store := testObjectStoreFixture("os-i07-store")
	if err := cli.Create(ctx, store); err != nil {
		t.Fatalf("creating the ObjectStore: %v", err)
	}
	key := client.ObjectKeyFromObject(store)
	r := &ObjectStoreReconciler{Client: cli, Scheme: cli.Scheme()}
	reconcileStoreUntil(t, r, key, func(o *objectstorev1alpha1.ObjectStore) bool {
		return o.Status.Phase == osPhaseProvisioning
	})

	var before appsv1.Deployment
	if err := cli.Get(ctx, key, &before); err != nil {
		t.Fatalf("reading the Deployment: %v", err)
	}

	if _, err := r.Reconcile(ctx, ctrlRequestKey(key)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var after appsv1.Deployment
	if err := cli.Get(ctx, key, &after); err != nil {
		t.Fatalf("reading the Deployment back: %v", err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("Deployment resourceVersion changed from %s to %s on a reconcile pass with nothing to change",
			before.ResourceVersion, after.ResourceVersion)
	}
}

// U-16 (moved here from the unit class): spec.image naming a registry
// outside the three the CRD's own Pattern allows is rejected at admission,
// which only a real apiserver enforces. A fake client applies no OpenAPI
// schema validation at all, so this cannot be proven as a pure unit test the
// way the test plan first scoped it.
func TestObjectStoreImageOutsideTheAllowedRegistriesIsRejectedAtCreate(t *testing.T) {
	cli := apiServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := testObjectStoreFixture("os-i16-store", func(o *objectstorev1alpha1.ObjectStore) {
		o.Spec.Image = "ghcr.io/simplyblock/gateway:main"
	})
	if err := cli.Create(ctx, store); err == nil {
		t.Fatal("Create succeeded with an image outside the three allowed registries, want it rejected")
	}
}
