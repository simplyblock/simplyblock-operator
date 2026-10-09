// Tests for the data-protection band's three admission guards: who may write a
// backup record, what a policy and an operation have to name, and when the
// record of a running restore may be withdrawn.
//
// Two assertions here are the ones worth protecting. A user may not delete a
// StorageBackup, because doing so would hide a copy that is still in the bucket
// and free nothing. And a restore may not name a claim that exists, because
// adopting one would replace that workload's data with the backup's.

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// backupNamespace is where the backups under test live, which is a storage
// cluster's namespace and deliberately not the operator's: the identity check is
// about who the caller is, not about where the object sits.
const backupNamespace = "sb"

// The namespaces a cross-namespace restore spreads its references across.
const (
	infraNamespace = "infra"
	teamNamespace  = "team-b"
)

func backupValidator(t *testing.T, objs ...client.Object) *StorageBackupValidator {
	t.Helper()
	return &StorageBackupValidator{
		Client:            fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build(),
		OperatorNamespace: validatorOperatorNamespace,
	}
}

func backupRequest(op admissionv1.Operation, username string) admission.Request {
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: op,
		Namespace: backupNamespace,
		Name:      "44444444-4444-4444-4444-444444444444",
		UserInfo:  authenticationv1.UserInfo{Username: username},
	}}
}

func liveBackupNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: backupNamespace}}
}

func terminatingBackupNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: backupNamespace},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
	}
}

// A backup object records a copy the operator found in the store, so one written
// by hand would name a copy that does not exist and every restore addressing it
// would fail against nothing.
func TestAUserMayNotCreateAStorageBackup(t *testing.T) {
	v := backupValidator(t, liveBackupNamespace())

	resp := v.Handle(context.Background(), backupRequest(admissionv1.Create, "kubernetes-admin"))
	if resp.Allowed {
		t.Error("a user was allowed to create a StorageBackup")
	}
	if !strings.Contains(resp.Result.Message, "StorageBackupPolicy") {
		t.Errorf("the refusal does not say how to get a backup taken: %q", resp.Result.Message)
	}
}

// The refusal that matters most. Deleting a backup record does not delete the
// copy, so a delete that was allowed would hide a restorable backup and free
// nothing.
func TestAUserMayNotDeleteAStorageBackup(t *testing.T) {
	v := backupValidator(t, liveBackupNamespace())

	resp := v.Handle(context.Background(), backupRequest(admissionv1.Delete, "kubernetes-admin"))
	if resp.Allowed {
		t.Error("a user was allowed to delete a StorageBackup")
	}
	if !strings.Contains(resp.Result.Message, "would not delete the copy") {
		t.Errorf("the refusal does not say what deleting the record does not do: %q", resp.Result.Message)
	}
}

func TestTheOperatorMayWriteAStorageBackup(t *testing.T) {
	v := backupValidator(t, liveBackupNamespace())

	for _, op := range []admissionv1.Operation{admissionv1.Create, admissionv1.Delete} {
		req := backupRequest(op, "system:serviceaccount:"+validatorOperatorNamespace+":simplyblock-operator")
		if resp := v.Handle(context.Background(), req); !resp.Allowed {
			t.Errorf("the operator was refused a %s: %q", op, resp.Result.Message)
		}
	}
}

// Refusing the namespace controller's delete would leave the namespace in
// Terminating forever, with no way out but removing the webhook by hand.
func TestNamespaceTeardownMayDeleteAStorageBackup(t *testing.T) {
	v := backupValidator(t, terminatingBackupNamespace())

	resp := v.Handle(context.Background(),
		backupRequest(admissionv1.Delete, "system:serviceaccount:kube-system:namespace-controller"))
	if !resp.Allowed {
		t.Errorf("the namespace teardown was refused: %q", resp.Result.Message)
	}
}

func policyValidator(t *testing.T, objs ...client.Object) *StorageBackupPolicyValidator {
	t.Helper()
	return &StorageBackupPolicyValidator{
		Client: fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build(),
	}
}

func createRequest(t *testing.T, object runtime.Object) admission.Request {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("encode the object under test: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: backupNamespace,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func clusterObject() *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: backupNamespace},
	}
}

// spec.clusterRef is immutable, so a wrong one could only ever be deleted rather
// than fixed. Refusing at creation is what asks for the rewrite immediately and
// at no cost.
func TestAPolicyNamingNoClusterIsRefused(t *testing.T) {
	v := policyValidator(t)

	policy := &simplyblockv1alpha2.StorageBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: backupNamespace},
		Spec:       simplyblockv1alpha2.StorageBackupPolicySpec{ClusterRef: "no-such-cluster"},
	}
	if resp := v.Handle(context.Background(), createRequest(t, policy)); resp.Allowed {
		t.Error("a policy naming a cluster that does not exist was admitted")
	}
}

// A policy with no selector covers nothing, and that is a policy somebody may be
// about to label claims for rather than a mistake to refuse.
func TestAPolicyWithNoSelectorIsAdmitted(t *testing.T) {
	v := policyValidator(t, clusterObject())

	policy := &simplyblockv1alpha2.StorageBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: backupNamespace},
		Spec:       simplyblockv1alpha2.StorageBackupPolicySpec{ClusterRef: "production"},
	}
	if resp := v.Handle(context.Background(), createRequest(t, policy)); !resp.Allowed {
		t.Errorf("a policy with no selector was refused: %q", resp.Result.Message)
	}
}

func opsValidator(t *testing.T, objs ...client.Object) *StorageBackupOpsValidator {
	t.Helper()
	return &StorageBackupOpsValidator{
		Client: fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build(),
	}
}

func availableBackupObject() *simplyblockv1alpha2.StorageBackup {
	return &simplyblockv1alpha2.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: backupNamespace},
		Status: simplyblockv1alpha2.StorageBackupStatus{
			Phase: simplyblockv1alpha2.StorageBackupPhaseAvailable,
		},
	}
}

// The pool is seeded at v1alpha2 because that is the version an API server
// stores and serves. See TestARestoreWithResolvableReferencesIsAdmitted for what
// seeding the retired version concealed.
func poolObject() *simplyblockv1alpha2.StoragePool {
	return &simplyblockv1alpha2.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: backupNamespace},
		Spec:       simplyblockv1alpha2.StoragePoolSpec{ClusterRef: "production"},
	}
}

func restoreOpsObject() *simplyblockv1alpha2.StorageBackupOps {
	return &simplyblockv1alpha2.StorageBackupOps{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-1", Namespace: backupNamespace},
		Spec: simplyblockv1alpha2.StorageBackupOpsSpec{
			ClusterRef: simplyblockv1alpha2.NamespacedReference{Name: "production"},
			BackupRef:  "backup-1",
			Action:     simplyblockv1alpha2.StorageBackupOpsActionRestore,
			Restore: &simplyblockv1alpha2.RestoreSpec{
				Claim:      simplyblockv1alpha2.NamespacedReference{Name: "restored-claim"},
				TargetPool: "pool-a",
			},
		},
	}
}

// Regression: 2026-09-11-backupops-pool-check-reads-retired-version. The guard
// resolved the target pool as a v1alpha1 StoragePool after v1alpha2 became the
// stored version. A read of the retired version is answered only by the
// conversion webhook, which a fresh install does not deploy, so every restore
// was denied for naming a pool that exists. This passed throughout, because the
// fixture seeded the same retired version the guard read.
func TestARestoreWithResolvableReferencesIsAdmitted(t *testing.T) {
	v := opsValidator(t, clusterObject(), availableBackupObject(), poolObject())

	resp := v.Handle(context.Background(), createRequest(t, restoreOpsObject()))
	if !resp.Allowed {
		t.Errorf("a restore naming objects that all exist was refused: %q", resp.Result.Message)
	}
}

// The check standing between a restore and overwriting a running workload's
// data. It cannot close the race with a claim created afterward, which the
// operation's own first step catches, and catching the ordinary mistake here is
// still worth it.
func TestARestoreOntoAnExistingClaimIsRefused(t *testing.T) {
	occupied := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "restored-claim", Namespace: backupNamespace},
	}
	v := opsValidator(t, clusterObject(), availableBackupObject(), poolObject(), occupied)

	resp := v.Handle(context.Background(), createRequest(t, restoreOpsObject()))
	if resp.Allowed {
		t.Fatal("a restore onto a claim that already exists was admitted")
	}
	if !strings.Contains(resp.Result.Message, "never adopts") {
		t.Errorf("the refusal does not say why: %q", resp.Result.Message)
	}
}

func TestARestoreOfAFailedBackupIsRefused(t *testing.T) {
	broken := availableBackupObject()
	broken.Status.Phase = simplyblockv1alpha2.StorageBackupPhaseFailed
	v := opsValidator(t, clusterObject(), broken, poolObject())

	if resp := v.Handle(context.Background(), createRequest(t, restoreOpsObject())); resp.Allowed {
		t.Error("a restore of a backup with no copy behind it was admitted")
	}
}

func TestARestoreIntoAPoolThisClusterLacksIsRefused(t *testing.T) {
	v := opsValidator(t, clusterObject(), availableBackupObject())

	if resp := v.Handle(context.Background(), createRequest(t, restoreOpsObject())); resp.Allowed {
		t.Error("a restore naming a pool that does not exist was admitted")
	}
}

func deleteOpsRequest(t *testing.T, ops *simplyblockv1alpha2.StorageBackupOps) admission.Request {
	t.Helper()
	raw, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("encode the operation: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
		Namespace: ops.Namespace,
		Name:      ops.Name,
		OldObject: runtime.RawExtension{Raw: raw},
	}}
}

// A restore that has created a logical volume is what finishes it. Deleting the
// record would not stop that work; it would remove the only account of it.
func TestDeletingARestorePastThePointOfNoReturnIsRefused(t *testing.T) {
	v := opsValidator(t)

	for _, step := range []simplyblockv1alpha2.StorageBackupOpsStep{
		// Restoring is included because the request that creates the volume is
		// made before the identifier can be persisted, so an operation here may
		// already have one and this object is the only thing that can find it.
		simplyblockv1alpha2.StorageBackupOpsStepRestoring,
		simplyblockv1alpha2.StorageBackupOpsStepAwaitingVolume,
		simplyblockv1alpha2.StorageBackupOpsStepBinding,
	} {
		ops := restoreOpsObject()
		ops.Status.Phase = simplyblockv1alpha2.StorageBackupOpsPhaseRunning
		ops.Status.Step.State = string(step)

		resp := v.Handle(context.Background(), deleteOpsRequest(t, ops))
		if resp.Allowed {
			t.Errorf("deleting an operation at step %s was admitted", step)
			continue
		}
		if !strings.Contains(resp.Result.Message, "spec.abort") {
			t.Errorf("the refusal at step %s does not name the way to stop it: %q",
				step, resp.Result.Message)
		}
	}
}

// A restore that has created nothing is deleted freely, and so is a terminal
// one: withdrawing the record of finished work stops nothing.
func TestDeletingARestoreThatCreatedNothingIsAdmitted(t *testing.T) {
	v := opsValidator(t)

	// Validating has resolved names and created nothing, so there is nothing the
	// record is the only account of.
	early := restoreOpsObject()
	early.Status.Phase = simplyblockv1alpha2.StorageBackupOpsPhaseRunning
	early.Status.Step.State = string(simplyblockv1alpha2.StorageBackupOpsStepValidating)
	if resp := v.Handle(context.Background(), deleteOpsRequest(t, early)); !resp.Allowed {
		t.Errorf("deleting an operation at Validating was refused: %q", resp.Result.Message)
	}

	finished := restoreOpsObject()
	finished.Status.Phase = simplyblockv1alpha2.StorageBackupOpsPhaseSucceeded
	finished.Status.Step.State = string(simplyblockv1alpha2.StorageBackupOpsStepBinding)
	if resp := v.Handle(context.Background(), deleteOpsRequest(t, finished)); !resp.Allowed {
		t.Errorf("deleting a terminal operation was refused: %q", resp.Result.Message)
	}
}

// fakeReviewer answers an access review from a rule a test sets and records
// every question it was asked.
type fakeReviewer struct {
	asked []authorizationv1.ResourceAttributes
	users []authenticationv1.UserInfo
	deny  func(authorizationv1.ResourceAttributes) bool
	err   error
}

func (f *fakeReviewer) Allowed(
	_ context.Context, user authenticationv1.UserInfo, attributes authorizationv1.ResourceAttributes,
) (bool, error) {
	f.asked = append(f.asked, attributes)
	f.users = append(f.users, user)
	if f.err != nil {
		return false, f.err
	}
	return f.deny == nil || !f.deny(attributes), nil
}

// crossingOps is a restore whose operation, cluster, and claim are in three
// different namespaces, with the backup in the operator's.
func crossingOps() *simplyblockv1alpha2.StorageBackupOps {
	ops := restoreOpsObject()
	ops.Namespace = "apps"
	ops.Spec.ClusterRef.Namespace = infraNamespace
	ops.Spec.Restore.Claim.Namespace = teamNamespace
	return ops
}

func crossingWorld() []client.Object {
	cluster := clusterObject()
	cluster.Namespace = infraNamespace
	pool := poolObject()
	pool.Namespace = infraNamespace
	return []client.Object{cluster, availableBackupObject(), pool}
}

func crossingValidator(t *testing.T, reviewer AccessReviewer, objs ...client.Object) *StorageBackupOpsValidator {
	t.Helper()
	v := opsValidator(t, objs...)
	v.OperatorNamespace = backupNamespace
	v.Reviewer = reviewer
	return v
}

func crossingRequest(t *testing.T, ops *simplyblockv1alpha2.StorageBackupOps) admission.Request {
	t.Helper()
	req := createRequest(t, ops)
	req.UserInfo = authenticationv1.UserInfo{Username: "alice", Groups: []string{"team-a"}}
	return req
}

// Backup from anywhere, restore to anywhere: every reference may name another
// namespace, and the validator resolves each where it is.
func TestARestoreAcrossNamespacesIsAdmittedAndEachReferenceIsAuthorized(t *testing.T) {
	reviewer := &fakeReviewer{}
	v := crossingValidator(t, reviewer, crossingWorld()...)

	resp := v.Handle(context.Background(), crossingRequest(t, crossingOps()))

	if !resp.Allowed {
		t.Fatalf("a restore across namespaces was refused: %q", resp.Result.Message)
	}
	want := []authorizationv1.ResourceAttributes{
		{Verb: "get", Group: "storage.simplyblock.io", Resource: "storagebackups", Namespace: backupNamespace, Name: "backup-1"},
		{Verb: "get", Group: "storage.simplyblock.io", Resource: "storageclusters", Namespace: infraNamespace, Name: "production"},
		{Verb: "create", Resource: "persistentvolumeclaims", Namespace: teamNamespace},
	}
	if !reflect.DeepEqual(reviewer.asked, want) {
		t.Errorf("reviews asked = %+v, want %+v", reviewer.asked, want)
	}
	for _, user := range reviewer.users {
		if user.Username != "alice" || !slices.Contains(user.Groups, "team-a") {
			t.Errorf("a review was made for %+v, want the requesting user and their groups", user)
		}
	}
}

// Resolving a reference says nothing about whether the requester may use what it
// names, so each crossing is refused when the requester is not allowed to.
func TestAReferenceTheRequesterMayNotUseIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, resource, namespace string }{
		{"backup", "storagebackups", backupNamespace},
		{"cluster", "storageclusters", infraNamespace},
		{"claim namespace", "persistentvolumeclaims", teamNamespace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewer := &fakeReviewer{deny: func(a authorizationv1.ResourceAttributes) bool {
				return a.Resource == tc.resource
			}}
			v := crossingValidator(t, reviewer, crossingWorld()...)

			resp := v.Handle(context.Background(), crossingRequest(t, crossingOps()))

			if resp.Allowed {
				t.Fatalf("a restore using %s the requester may not use was admitted", tc.resource)
			}
			if !strings.Contains(resp.Result.Message, tc.resource) ||
				!strings.Contains(resp.Result.Message, tc.namespace) {
				t.Errorf("the refusal does not name %s in %s: %q", tc.resource, tc.namespace, resp.Result.Message)
			}
		})
	}
}

// An operation that stays in its own namespace is what every restore was before
// the fields existed, and it needs no review.
func TestARestoreWithinOneNamespaceAsksForNoReview(t *testing.T) {
	reviewer := &fakeReviewer{}
	v := opsValidator(t, clusterObject(), availableBackupObject(), poolObject())
	v.OperatorNamespace = backupNamespace
	v.Reviewer = reviewer

	resp := v.Handle(context.Background(), createRequest(t, restoreOpsObject()))

	if !resp.Allowed {
		t.Fatalf("a restore within one namespace was refused: %q", resp.Result.Message)
	}
	if len(reviewer.asked) != 0 {
		t.Errorf("reviews asked = %+v, want none", reviewer.asked)
	}
}

// A refusal that said "no such backup" to somebody who may not read backups
// would tell them which ones exist.
func TestAnUnauthorizedReferenceIsRefusedBeforeItIsResolved(t *testing.T) {
	reviewer := &fakeReviewer{deny: func(authorizationv1.ResourceAttributes) bool { return true }}
	cluster := clusterObject()
	cluster.Namespace = infraNamespace
	v := crossingValidator(t, reviewer, cluster) // no backup, no pool

	resp := v.Handle(context.Background(), crossingRequest(t, crossingOps()))

	if resp.Allowed {
		t.Fatal("an unauthorized restore was admitted")
	}
	if strings.Contains(resp.Result.Message, "does not name") {
		t.Errorf("the refusal reveals whether the object exists: %q", resp.Result.Message)
	}
}

func TestARestoreIsRefusedWhenTheAuthorizerCannotBeAsked(t *testing.T) {
	reviewer := &fakeReviewer{err: errors.New("the API server is unreachable")}
	v := crossingValidator(t, reviewer, crossingWorld()...)

	if resp := v.Handle(context.Background(), crossingRequest(t, crossingOps())); resp.Allowed {
		t.Error("a restore was admitted without an answer from the authorizer")
	}
}

func TestACrossNamespaceRestoreResolvesThePoolInTheClustersNamespace(t *testing.T) {
	cluster := clusterObject()
	cluster.Namespace = infraNamespace
	// The pool is in the operation's namespace, where the restore does not look.
	pool := poolObject()
	pool.Namespace = "apps"
	v := crossingValidator(t, &fakeReviewer{}, cluster, availableBackupObject(), pool)

	resp := v.Handle(context.Background(), crossingRequest(t, crossingOps()))

	if resp.Allowed {
		t.Fatal("a pool in the wrong namespace was accepted")
	}
	if !strings.Contains(resp.Result.Message, `"infra"`) {
		t.Errorf("the refusal does not name the namespace it looked in: %q", resp.Result.Message)
	}
}

func TestACrossNamespaceRestoreRefusesAClaimThatExistsInTheClaimNamespace(t *testing.T) {
	occupied := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "restored-claim", Namespace: teamNamespace},
	}
	v := crossingValidator(t, &fakeReviewer{}, append(crossingWorld(), occupied)...)

	resp := v.Handle(context.Background(), crossingRequest(t, crossingOps()))

	if resp.Allowed {
		t.Fatal("a restore onto an existing claim in another namespace was admitted")
	}
	if !strings.Contains(resp.Result.Message, `"team-b"`) {
		t.Errorf("the refusal does not name the claim's namespace: %q", resp.Result.Message)
	}
}

// rawRestoreRequest is a create request whose references are written the way a
// manifest writes them, as objects with an optional namespace, so the test
// states the wire shape and not the Go type behind it.
func rawRestoreRequest(namespace, clusterRef, claim string) admission.Request {
	raw := fmt.Sprintf(`{
	  "apiVersion": "storage.simplyblock.io/v1alpha2", "kind": "StorageBackupOps",
	  "metadata": {"name": "restore-1", "namespace": %q},
	  "spec": {
	    "clusterRef": %s, "backupRef": "backup-1", "action": "Restore",
	    "restore": {"claim": %s, "targetPool": "pool-a"}
	  }}`, namespace, clusterRef, claim)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: namespace,
		UserInfo:  authenticationv1.UserInfo{Username: "alice"},
		Object:    runtime.RawExtension{Raw: []byte(raw)},
	}}
}

// A reference that names no namespace means the operation's own, so an operation
// that omits both stays where it is and asks for no review.
func TestAReferenceWithoutANamespaceMeansTheOperationsNamespace(t *testing.T) {
	reviewer := &fakeReviewer{}
	v := opsValidator(t, clusterObject(), availableBackupObject(), poolObject())
	v.OperatorNamespace = backupNamespace
	v.Reviewer = reviewer

	resp := v.Handle(context.Background(), rawRestoreRequest(backupNamespace,
		`{"name": "production"}`, `{"name": "restored-claim"}`))

	if !resp.Allowed {
		t.Fatalf("a restore whose references omit the namespace was refused: %q", resp.Result.Message)
	}
	if len(reviewer.asked) != 0 {
		t.Errorf("reviews asked = %+v, want none", reviewer.asked)
	}
}

// The default is the operation's namespace and not the operator's: the cluster
// is looked up where the operation is, and the refusal says where.
func TestAClusterRefWithoutANamespaceIsResolvedInTheOperationsNamespace(t *testing.T) {
	cluster := clusterObject()
	cluster.Namespace = infraNamespace
	pool := poolObject()
	pool.Namespace = infraNamespace
	v := crossingValidator(t, &fakeReviewer{}, cluster, availableBackupObject(), pool)

	resp := v.Handle(context.Background(), rawRestoreRequest("apps",
		`{"name": "production"}`, `{"name": "restored-claim"}`))

	if resp.Allowed {
		t.Fatal("a cluster in another namespace was resolved without being named")
	}
	if !strings.Contains(resp.Result.Message, `"apps"`) {
		t.Errorf("the refusal does not name the namespace it looked in: %q", resp.Result.Message)
	}
}

// A claim that names no namespace is checked, and authorized, in the
// operation's namespace.
func TestAClaimWithoutANamespaceIsCheckedInTheOperationsNamespace(t *testing.T) {
	occupied := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "restored-claim", Namespace: "apps"},
	}
	cluster := clusterObject()
	cluster.Namespace = infraNamespace
	pool := poolObject()
	pool.Namespace = infraNamespace
	reviewer := &fakeReviewer{}
	v := crossingValidator(t, reviewer, cluster, availableBackupObject(), pool, occupied)

	resp := v.Handle(context.Background(), rawRestoreRequest("apps",
		`{"name": "production", "namespace": "infra"}`, `{"name": "restored-claim"}`))

	if resp.Allowed {
		t.Fatal("a restore onto a claim in the operation's namespace was admitted")
	}
	if !strings.Contains(resp.Result.Message, `"apps"`) {
		t.Errorf("the refusal does not name the claim's namespace: %q", resp.Result.Message)
	}
}
