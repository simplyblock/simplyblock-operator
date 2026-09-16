// The reconciler's branches: the singleton, the two sources, the install's
// progression, and the deletion hold.
//
// The fake client carries no RESTMapper that knows the FoundationDB kinds, so
// the tests that drive a managed install past the prerequisite check call the
// install path directly. What the prerequisite check itself does is tested
// against a reconciler with no mapping, which is the state of a cluster that has
// not been given the CRDs.

package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// A ControlPlane under any other name is ignored and sits inert, which is the
// singleton enforced by convention. Reconciling one would install a second
// control plane beside the first.
func TestAControlPlaneThatIsNotTheSingletonIsIgnored(t *testing.T) {
	other := localControlPlane()
	other.Name = "a-second-one"
	c := newClient(t, other)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(other),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want 0: an ignored object is not looked at again",
			result.RequeueAfter)
	}

	var after simplyblockv1alpha2.ControlPlane
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(other), &after); err != nil {
		t.Fatalf("read the object back: %v", err)
	}
	if after.Status.Phase != "" {
		t.Errorf("status.phase = %s, want nothing written at all", after.Status.Phase)
	}
	if len(after.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want none on an object the controller ignores", after.Finalizers)
	}
}

// A ControlPlane is one per Kubernetes cluster, not one per namespace. Two of
// them reconciling at once apply the same fixed-name cluster-scoped RBAC under
// the same managed-by label, so each overwrites the other's and either one's
// deletion takes away what the other needs.
//
// The older object holds the deployment, and the younger reports that it does
// not. That is the rule SimplyblockDriver uses for the same reason, and the two
// have to agree about which object is the second.
func TestASecondControlPlaneInAnotherNamespaceDoesNotInstall(t *testing.T) {
	ctx := context.Background()

	holder := localControlPlane()
	holder.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))

	second := localControlPlane()
	second.Namespace = "simplyblock-second"
	second.CreationTimestamp = metav1.NewTime(time.Now())

	c := newClient(t, holder, second)
	recorder := &recordingRecorder{}
	r := &ControlPlaneReconciler{
		Client: c, Scheme: testScheme(t), Recorder: recorder,
		Prober: &stubProber{ready: true},
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(second),
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.InNamespace(second.Namespace)); err != nil {
		t.Fatalf("list the second namespace: %v", err)
	}
	if len(deployments.Items) != 0 {
		t.Errorf("the second control plane installed %d workloads, and the first holds the "+
			"cluster-scoped objects they would overwrite", len(deployments.Items))
	}

	var after simplyblockv1alpha2.ControlPlane
	if err := c.Get(ctx, client.ObjectKeyFromObject(second), &after); err != nil {
		t.Fatalf("read the second control plane back: %v", err)
	}
	if !strings.Contains(after.Status.Message, holder.Namespace) {
		t.Errorf("status.message = %q, want it to name the namespace that holds the "+
			"deployment", after.Status.Message)
	}
	if recorder.count(DuplicateControlPlane) == 0 {
		t.Error("no DuplicateControlPlane event: nothing says why this object does nothing")
	}
}

// The holder itself still installs. A rule that refused both would take a
// working deployment down on the first reconcile after somebody added a second
// object by mistake.
func TestTheOlderControlPlaneStillHoldsTheDeployment(t *testing.T) {
	holder := localControlPlane()
	holder.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))

	second := localControlPlane()
	second.Namespace = "simplyblock-second"
	second.CreationTimestamp = metav1.NewTime(time.Now())

	c := newClient(t, holder, second)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	key, err := r.deploymentHolder(context.Background())
	if err != nil {
		t.Fatalf("deploymentHolder: %v", err)
	}
	if key.Namespace != holder.Namespace {
		t.Errorf("the holder is %s/%s, want the older object in %s",
			key.Namespace, key.Name, holder.Namespace)
	}
}

// A remote control plane is resolved, probed, and reported, and the operator
// applies nothing. status.endpoint echoes what the spec said, which is the point
// of the field: a reader asks status.endpoint either way and does not have to
// know which mode the deployment is in.
func TestARemoteControlPlaneIsProbedAndNothingIsInstalled(t *testing.T) {
	const endpoint = "https://sb-control.example.com:5000"
	cp := managedControlPlane(endpoint)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-token", Namespace: testNamespace},
		Data:       map[string][]byte{"token": []byte("a-bearer-token")},
	}

	c := newClient(t, cp, secret)
	prober := &stubProber{ready: true, version: "26.2.8"}
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t), Prober: prober}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cp),
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var after simplyblockv1alpha2.ControlPlane
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cp), &after); err != nil {
		t.Fatalf("read the object back: %v", err)
	}

	if after.Status.Phase != simplyblockv1alpha2.ControlPlanePhaseAvailable {
		t.Errorf("status.phase = %s, want Available", after.Status.Phase)
	}
	if after.Status.Endpoint != endpoint {
		t.Errorf("status.endpoint = %q, want the spec's %q", after.Status.Endpoint, endpoint)
	}
	if after.Status.Version != "26.2.8" {
		t.Errorf("status.version = %q, want what the control plane reported", after.Status.Version)
	}
	if len(after.Status.Components) != 0 {
		t.Errorf("status.components = %v, want empty: the operator owns no pods there",
			after.Status.Components)
	}
	if after.Status.LastChecked == nil {
		t.Error("status.lastChecked is unset after a probe ran")
	}
}

// A remote control plane may name no credentials Secret, and that is the
// shape the chart writes when it installs the control plane itself: the endpoint
// is a ClusterIP Service in the same namespace, the readiness probe there is
// unauthenticated, and there is no static token to point at.
//
// It is the field that lets the chart hand the install back: the CR names a
// remote source, and the operator resolves it rather than installing.
func TestARemoteControlPlaneMayNameNoCredentials(t *testing.T) {
	ctx := context.Background()
	const endpoint = "http://simplyblock-webappapi.simplyblock.svc.cluster.local:5000"

	cp := managedControlPlane(endpoint)
	cp.Spec.Source.Managed.CredentialsSecretRef = nil

	c := newClient(t, cp)
	prober := &stubProber{ready: true}
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t), Prober: prober}

	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cp),
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var after simplyblockv1alpha2.ControlPlane
	if err := c.Get(ctx, client.ObjectKeyFromObject(cp), &after); err != nil {
		t.Fatalf("read the object back: %v", err)
	}
	if after.Status.Phase != simplyblockv1alpha2.ControlPlanePhaseAvailable {
		t.Errorf("status.phase = %s (%q), want Available", after.Status.Phase, after.Status.Message)
	}
	if after.Status.Endpoint != endpoint {
		t.Errorf("status.endpoint = %q, want %q", after.Status.Endpoint, endpoint)
	}
	if prober.readyCalls == 0 {
		t.Error("the endpoint was never probed")
	}
}

// A credentials Secret that is named and absent is still an error. Naming one
// states that the control plane needs it, and the probe is refused until it is
// there.
func TestANamedButMissingCredentialsSecretIsStillAnError(t *testing.T) {
	cp := managedControlPlane("https://sb-control.example.com:5000")
	c := newClient(t, cp)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	_, err := resolveManaged(context.Background(), c, cp)
	if err == nil {
		t.Fatal("a named Secret that does not exist was accepted")
	}
	var credentials *credentialsError
	if !errorsAs(err, &credentials) {
		t.Errorf("err = %v, want a credentials error so the event names the right cause", err)
	}
	_ = r
}

// A credentials Secret that does not exist is a different problem from an
// endpoint that does not answer, and it is reported as one: the phase is
// Unavailable either way, and the message names the Secret.
func TestAMissingCredentialsSecretIsReportedAsSuch(t *testing.T) {
	cp := managedControlPlane("https://sb-control.example.com:5000")
	c := newClient(t, cp)
	prober := &stubProber{ready: true}
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t), Prober: prober}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cp),
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var after simplyblockv1alpha2.ControlPlane
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cp), &after); err != nil {
		t.Fatalf("read the object back: %v", err)
	}
	if after.Status.Phase != simplyblockv1alpha2.ControlPlanePhaseUnavailable {
		t.Errorf("status.phase = %s, want Unavailable", after.Status.Phase)
	}
	if !strings.Contains(after.Status.Message, "cp-token") {
		t.Errorf("status.message = %q, want it to name the Secret", after.Status.Message)
	}
	if prober.readyCalls != 0 {
		t.Error("the endpoint was probed although there was no token to probe it with")
	}
}

// An endpoint that resolves inside the operator's own pod is refused. The spec's
// pattern admits it, so this is the guard that stops an operator being pointed
// at itself.
func TestALoopbackEndpointIsRefused(t *testing.T) {
	for _, endpoint := range []string{
		"http://localhost:5000",
		"http://127.0.0.1:5000",
		"https://169.254.169.254/latest",
	} {
		if err := validateEndpoint(endpoint); err == nil {
			t.Errorf("%s was admitted, and it does not name a control plane", endpoint)
		}
	}
	for _, endpoint := range []string{
		"https://sb-control.example.com:5000",
		"http://10.0.0.5:5000",
	} {
		if err := validateEndpoint(endpoint); err != nil {
			t.Errorf("%s was refused: %v", endpoint, err)
		}
	}
}

// A managed install holds with a named prerequisite where the FoundationDB kinds
// are not served, rather than failing against the API server on every pass. The
// CRDs are the chart's to apply, and creating a FoundationDBCluster against a
// group the API server does not know is an error that reconciling does not fix.
func TestAMissingFoundationDBGroupHoldsTheInstallWithAReason(t *testing.T) {
	cp := localControlPlane()
	c := newClient(t, cp)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cp),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("the install was not requeued, so it would never notice the CRDs arriving")
	}

	var after simplyblockv1alpha2.ControlPlane
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cp), &after); err != nil {
		t.Fatalf("read the object back: %v", err)
	}
	if after.Status.Phase != simplyblockv1alpha2.ControlPlanePhaseInstalling {
		t.Errorf("status.phase = %s, want Installing", after.Status.Phase)
	}
	if !strings.Contains(after.Status.Message, fdbGroup) {
		t.Errorf("status.message = %q, want it to name the group that is missing",
			after.Status.Message)
	}
}

// The finalizer is taken before anything is installed, so that an object deleted
// mid-install still goes through the hold rather than being collected while its
// database is coming up.
func TestTheFinalizerIsTakenBeforeAnythingIsInstalled(t *testing.T) {
	cp := localControlPlane()
	c := newClient(t, cp)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cp),
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var after simplyblockv1alpha2.ControlPlane
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cp), &after); err != nil {
		t.Fatalf("read the object back: %v", err)
	}
	if !containsString(after.Finalizers, FinalizerControlPlane) {
		t.Errorf("finalizers = %v, want %s", after.Finalizers, FinalizerControlPlane)
	}
}

// Deleting a ControlPlane while clusters still exist is held, not failed:
// removing the clusters resolves it, and nothing else can. The data those
// clusters describe lives in the FoundationDB this object's deletion would take
// with it.
func TestDeletionIsHeldWhileClustersStillExist(t *testing.T) {
	cp := localControlPlane()
	cp.Finalizers = []string{FinalizerControlPlane}
	now := metav1.Now()
	cp.DeletionTimestamp = &now

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: testNamespace},
	}
	c := newClient(t, cp, cluster)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cp),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("the hold was not requeued, so removing the cluster would not release it")
	}

	var after simplyblockv1alpha2.ControlPlane
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cp), &after); err != nil {
		t.Fatalf("the object was collected while a cluster still existed: %v", err)
	}
	if !containsString(after.Finalizers, FinalizerControlPlane) {
		t.Errorf("finalizers = %v, want the hold still in place", after.Finalizers)
	}
	if !strings.Contains(after.Status.Message, "StorageCluster") {
		t.Errorf("status.message = %q, want it to say what is holding the deletion",
			after.Status.Message)
	}
}

// With no clusters left, the hold releases and the cluster-scoped objects this
// controller marked go with it.
func TestDeletionReleasesOnceTheClustersAreGone(t *testing.T) {
	ctx := context.Background()
	cp := localControlPlane()
	cp.Finalizers = []string{FinalizerControlPlane}
	now := metav1.Now()
	cp.DeletionTimestamp = &now

	// A cluster-scoped object this controller marked, and one it did not.
	ours := fdbManagerClusterRoleObject()
	ours.Labels = map[string]string{managedByLabel: managedByValue}
	theirs := serviceReaderClusterRole()
	theirs.Labels = map[string]string{managedByLabel: "somebody-else"}

	c := newClient(t, cp, ours, theirs)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cp),
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var after simplyblockv1alpha2.ControlPlane
	err := c.Get(ctx, client.ObjectKeyFromObject(cp), &after)
	if err == nil && containsString(after.Finalizers, FinalizerControlPlane) {
		t.Error("the finalizer is still held with no clusters left")
	}
}

// The event marks the arrival at a phase rather than the phase itself, which
// bounds one outage to one event rather than one per thirty-second probe.
func TestTheEventMarksTheTransitionRatherThanTheState(t *testing.T) {
	recorder := &recordingRecorder{}
	cp := localControlPlane()
	cp.Status.Phase = simplyblockv1alpha2.ControlPlanePhaseUnavailable
	r := &ControlPlaneReconciler{Recorder: recorder}

	r.announce(cp, simplyblockv1alpha2.ControlPlanePhaseUnavailable, "still down")
	if len(recorder.reasons) != 0 {
		t.Errorf("an event was emitted for a phase that did not change: %v", recorder.reasons)
	}

	r.announce(cp, simplyblockv1alpha2.ControlPlanePhaseAvailable, "")
	if len(recorder.reasons) != 1 || recorder.reasons[0] != ControlPlaneReady {
		t.Errorf("reasons = %v, want one %s on the recovery", recorder.reasons, ControlPlaneReady)
	}
}

// containsString is slices.Contains for a finalizer list, spelled here so the
// assertions read as what they check.
func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
