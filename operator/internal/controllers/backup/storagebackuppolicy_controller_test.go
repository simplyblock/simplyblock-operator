// Tests for StorageBackupPolicy: what the selector covers, and what attaching
// and detaching does to the control plane.
//
// The selector's default is the assertion worth having. An absent selector
// selects nothing, which is the opposite of what an empty metav1.LabelSelector
// means in most Kubernetes APIs, so it is exactly the rule somebody will
// "correct" later without noticing that the cost of getting it wrong is a bill
// nobody sees.

package backup

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const testPolicyID = "66666666-6666-6666-6666-666666666666"

func policyObject(selector *metav1.LabelSelector) *simplyblockv1alpha2.StorageBackupPolicy {
	return &simplyblockv1alpha2.StorageBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "nightly",
			Namespace:  testNamespace,
			Finalizers: []string{policyFinalizer},
		},
		Spec: simplyblockv1alpha2.StorageBackupPolicySpec{
			ClusterRef:    testClusterCR,
			ClaimSelector: selector,
			Schedule:      "24h,7",
		},
	}
}

// coveredClaim is a bound claim whose volume this cluster backs, labeled the
// way the policy's selector matches.
func coveredClaim(name, lvolID string, labels map[string]string) []client.Object {
	volumeName := "pv-" + name
	return []client.Object{
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: volumeName},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: volumeName},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						VolumeHandle: testClusterID + ":" + testPoolID + ":" + lvolID,
					},
				},
			},
		},
	}
}

func policyReconciler(
	t *testing.T, api BackupClient, objs ...client.Object,
) *StorageBackupPolicyReconciler {
	t.Helper()
	return &StorageBackupPolicyReconciler{
		Client:   testClient(t, objs...),
		Scheme:   testScheme(t),
		Recorder: testRecorder(),
		API:      api,
	}
}

func policyRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "nightly"}}
}

func TestPolicyAttachesTheClaimsItsSelectorMatches(t *testing.T) {
	api := &fakeControlPlane{createdID: testPolicyID}
	objs := make([]client.Object, 0, 6)
	objs = append(objs, testClusterObject(), policyObject(&metav1.LabelSelector{
		MatchLabels: map[string]string{"backup": "nightly"},
	}))
	objs = append(objs, coveredClaim("wanted", testLvolID, map[string]string{"backup": "nightly"})...)
	objs = append(objs, coveredClaim("ignored", "99999999-9999-9999-9999-999999999999", nil)...)

	r := policyReconciler(t, api, objs...)
	if _, err := r.Reconcile(context.Background(), policyRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !slices.Equal(api.attaches, []string{testLvolID}) {
		t.Errorf("attached %v, want only the matching claim's volume", api.attaches)
	}

	var policy simplyblockv1alpha2.StorageBackupPolicy
	if err := r.Get(context.Background(), policyRequest().NamespacedName, &policy); err != nil {
		t.Fatal(err)
	}
	if got := policy.Status.Phase; got != simplyblockv1alpha2.StorageBackupPolicyPhaseActive {
		t.Errorf("phase = %q, want Active", got)
	}
	if len(policy.Status.AttachedClaims) != 1 || policy.Status.AttachedClaims[0].Name != "wanted" {
		t.Errorf("attachedClaims = %+v, want the one matching claim", policy.Status.AttachedClaims)
	}
	if policy.Status.PolicyID != testPolicyID {
		t.Errorf("policyID = %q, want the id the control plane assigned", policy.Status.PolicyID)
	}
}

// The default that makes the whole kind safe. A policy with no selector covers
// nothing, so it can be written before the claims it will cover exist without
// backing up everything in the namespace in the meantime.
func TestAPolicyWithNoSelectorCoversNothing(t *testing.T) {
	api := &fakeControlPlane{createdID: testPolicyID}
	objs := make([]client.Object, 0, 4)
	objs = append(objs, testClusterObject(), policyObject(nil))
	objs = append(objs, coveredClaim("would-have-matched", testLvolID, nil)...)

	r := policyReconciler(t, api, objs...)
	if _, err := r.Reconcile(context.Background(), policyRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(api.attaches) != 0 {
		t.Errorf("a policy with no selector attached %v", api.attaches)
	}
}

// A claim that stops matching is detached, and the copies already taken are
// kept: detaching governs whether new backups are taken, and retention governs
// how many are kept, and conflating them would make removing a label a
// data-deletion event.
func TestAClaimThatStopsMatchingIsDetached(t *testing.T) {
	api := &fakeControlPlane{createdID: testPolicyID}

	policy := policyObject(&metav1.LabelSelector{MatchLabels: map[string]string{"backup": "nightly"}})
	policy.Status = simplyblockv1alpha2.StorageBackupPolicyStatus{
		Phase:     simplyblockv1alpha2.StorageBackupPolicyPhaseActive,
		ClusterID: testClusterID,
		PolicyID:  testPolicyID,
		AttachedClaims: []simplyblockv1alpha2.AttachedClaim{
			{Name: "was-covered", LvolID: testLvolID},
		},
	}

	objs := make([]client.Object, 0, 4)
	objs = append(objs, testClusterObject(), policy)
	// The claim is still there, with the label removed.
	objs = append(objs, coveredClaim("was-covered", testLvolID, nil)...)

	r := policyReconciler(t, api, objs...)
	if _, err := r.Reconcile(context.Background(), policyRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !slices.Equal(api.detaches, []string{testLvolID}) {
		t.Errorf("detached %v, want the volume of the claim that stopped matching", api.detaches)
	}

	var reconciled simplyblockv1alpha2.StorageBackupPolicy
	if err := r.Get(context.Background(), policyRequest().NamespacedName, &reconciled); err != nil {
		t.Fatal(err)
	}
	if len(reconciled.Status.AttachedClaims) != 0 {
		t.Errorf("attachedClaims = %+v, want empty", reconciled.Status.AttachedClaims)
	}
}

// A claim provisioned by another cluster matches the selector and cannot be
// backed up by this policy, because this control plane has never heard of its
// volume. Skipping it is right; failing the whole policy is not.
func TestAClaimFromAnotherClusterIsSkippedRatherThanFatal(t *testing.T) {
	api := &fakeControlPlane{createdID: testPolicyID}
	labels := map[string]string{"backup": "nightly"}

	foreign := []client.Object{
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: testNamespace, Labels: labels},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-foreign"},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-foreign"},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						VolumeHandle: "99999999-9999-9999-9999-999999999999:" + testPoolID + ":" + testLvolID,
					},
				},
			},
		},
	}

	objs := make([]client.Object, 0, 6)
	objs = append(objs, testClusterObject(), policyObject(&metav1.LabelSelector{MatchLabels: labels}))
	objs = append(objs, foreign...)
	objs = append(objs, coveredClaim("ours", testLvolID, labels)...)

	r := policyReconciler(t, api, objs...)
	if _, err := r.Reconcile(context.Background(), policyRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !slices.Equal(api.attaches, []string{testLvolID}) {
		t.Errorf("attached %v, want only this cluster's claim", api.attaches)
	}
	var policy simplyblockv1alpha2.StorageBackupPolicy
	if err := r.Get(context.Background(), policyRequest().NamespacedName, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Status.Phase != simplyblockv1alpha2.StorageBackupPolicyPhaseActive {
		t.Errorf("phase = %q, want a policy covering its eligible claims to stay Active", policy.Status.Phase)
	}
}

// Deleting a policy detaches every volume before removing the policy itself. A
// delete that left attachments behind would leave the control plane taking
// copies for a policy nothing in Kubernetes accounts for.
func TestDeletingAPolicyDetachesBeforeItRemovesThePolicy(t *testing.T) {
	api := &fakeControlPlane{}
	now := metav1.Now()
	policy := policyObject(nil)
	policy.DeletionTimestamp = &now
	policy.Status = simplyblockv1alpha2.StorageBackupPolicyStatus{
		ClusterID: testClusterID,
		PolicyID:  testPolicyID,
		AttachedClaims: []simplyblockv1alpha2.AttachedClaim{
			{Name: "covered", LvolID: testLvolID},
		},
	}

	r := policyReconciler(t, api, testClusterObject(), policy)
	if _, err := r.Reconcile(context.Background(), policyRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !slices.Equal(api.detaches, []string{testLvolID}) {
		t.Errorf("detached %v while deleting, want the covered claim's volume", api.detaches)
	}
	if api.deletes != 1 {
		t.Errorf("the control-plane policy was deleted %d times, want once", api.deletes)
	}
}

// An explicitly empty selector is a different statement from an absent one, and
// it means what it means everywhere else in Kubernetes: everything. Somebody who
// wrote two empty braces asked for the whole namespace, and reading that as
// nothing would make the field impossible to use for what it plainly says.
func TestAnExplicitlyEmptySelectorCoversTheWholeNamespace(t *testing.T) {
	api := &fakeControlPlane{createdID: testPolicyID}
	objs := make([]client.Object, 0, 4)
	objs = append(objs, testClusterObject(), policyObject(&metav1.LabelSelector{}))
	objs = append(objs, coveredClaim("unlabeled", testLvolID, nil)...)

	r := policyReconciler(t, api, objs...)
	if _, err := r.Reconcile(context.Background(), policyRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !slices.Equal(api.attaches, []string{testLvolID}) {
		t.Errorf("attached %v, want the namespace's claim", api.attaches)
	}
}
