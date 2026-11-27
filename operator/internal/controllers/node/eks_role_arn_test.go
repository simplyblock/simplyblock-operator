// Whether the storage-node ServiceAccount carries the EKS role annotation the
// ControlPlane singleton states: added on creation, kept in sync on an
// existing account, and removed again if the role is unset, without
// disturbing any other annotation; and that a changed role is reported on
// the clusters whose pods keep the old one until restarted.

package node

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	atlaskube "github.com/simplyblock/atlas/kube"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const testEKSRoleARN = "arn:aws:iam::123456789012:role/simplyblock-backup"

func storageNodeServiceAccount(t *testing.T, r *StorageNodeWorkloadReconciler) *corev1.ServiceAccount {
	t.Helper()
	var account corev1.ServiceAccount
	key := client.ObjectKey{Namespace: opsNamespace, Name: "simplyblock-storage-node-sa"}
	if err := r.Get(context.Background(), key, &account); err != nil {
		t.Fatalf("read the ServiceAccount: %v", err)
	}
	return &account
}

// A ControlPlane singleton stating an EKS role gets that role annotated onto
// the ServiceAccount the storage-node pods run as, which is what lets EKS's
// pod-identity webhook inject it.
func TestTheStorageNodeServiceAccountCarriesTheControlPlanesEKSRole(t *testing.T) {
	cp := localControlPlaneSingleton()
	cp.Spec.Source.Local.EKSRoleARN = testEKSRoleARN
	cluster := aNamedCluster(secondCluster, secondClusterUUID, atlaskube.DerivedWorkloadNames(secondCluster))
	r := aNamesReconciler(t, cp, cluster)

	if err := r.reconcileRBAC(context.Background(), cluster); err != nil {
		t.Fatalf("reconcileRBAC: %v", err)
	}

	account := storageNodeServiceAccount(t, r)
	if got := account.Annotations[utils.EKSRoleARNAnnotation]; got != testEKSRoleARN {
		t.Errorf("annotation = %q, want %q", got, testEKSRoleARN)
	}
}

// An account created before the role was set, or under a different one, is
// corrected on the next reconcile rather than left as it was found.
func TestAnExistingServiceAccountsEKSRoleIsKeptInSync(t *testing.T) {
	cp := localControlPlaneSingleton()
	cp.Spec.Source.Local.EKSRoleARN = testEKSRoleARN
	cluster := aNamedCluster(secondCluster, secondClusterUUID, atlaskube.DerivedWorkloadNames(secondCluster))
	existing := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-storage-node-sa",
			Namespace: opsNamespace,
			Annotations: map[string]string{
				utils.EKSRoleARNAnnotation: "arn:aws:iam::123456789012:role/stale",
				"kept.example.com/other":   "untouched",
			},
		},
	}
	r := aNamesReconciler(t, cp, cluster, existing)

	if err := r.reconcileRBAC(context.Background(), cluster); err != nil {
		t.Fatalf("reconcileRBAC: %v", err)
	}

	account := storageNodeServiceAccount(t, r)
	if got := account.Annotations[utils.EKSRoleARNAnnotation]; got != testEKSRoleARN {
		t.Errorf("annotation = %q, want %q", got, testEKSRoleARN)
	}
	if account.Annotations["kept.example.com/other"] != "untouched" {
		t.Errorf("an unrelated annotation was disturbed: %v", account.Annotations)
	}
}

// A ControlPlane stating no EKS role, or none at all, leaves the account
// without the annotation.
func TestTheStorageNodeServiceAccountCarriesNoEKSRoleByDefault(t *testing.T) {
	cluster := aNamedCluster(secondCluster, secondClusterUUID, atlaskube.DerivedWorkloadNames(secondCluster))
	r := aNamesReconciler(t, cluster)

	if err := r.reconcileRBAC(context.Background(), cluster); err != nil {
		t.Fatalf("reconcileRBAC: %v", err)
	}

	account := storageNodeServiceAccount(t, r)
	if _, ok := account.Annotations[utils.EKSRoleARNAnnotation]; ok {
		t.Errorf("annotations = %v, want no EKS role annotation", account.Annotations)
	}
}

// staleRoleAccount is the storage-node ServiceAccount annotated with a role
// the ControlPlane no longer states.
func staleRoleAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:        "simplyblock-storage-node-sa",
		Namespace:   opsNamespace,
		Annotations: map[string]string{utils.EKSRoleARNAnnotation: "arn:aws:iam::123456789012:role/stale"},
	}}
}

// recordedReasons drains the recorder, counting each event reason.
func recordedReasons(recorder *events.FakeRecorder) map[string]int {
	reasons := map[string]int{}
	for {
		select {
		case e := <-recorder.Events:
			for _, field := range strings.Fields(e) {
				reasons[field]++
			}
		default:
			return reasons
		}
	}
}

// Running storage-node pods keep the role they were admitted with, and the
// operator does not restart them: a changed role is reported on every cluster
// whose pods run as the shared account, naming the rolling restart that
// applies it.
func TestAChangedEKSRoleWarnsEveryClusterOnTheAccount(t *testing.T) {
	cp := localControlPlaneSingleton()
	cp.Spec.Source.Local.EKSRoleARN = testEKSRoleARN
	first := aNamedCluster("first", firstClusterUUID, atlaskube.ConstantWorkloadNames())
	second := aNamedCluster(secondCluster, secondClusterUUID, atlaskube.DerivedWorkloadNames(secondCluster))
	r := aNamesReconciler(t, cp, first, second, staleRoleAccount())
	recorder := events.NewFakeRecorder(8)
	r.Recorder = recorder

	if err := r.reconcileRBAC(context.Background(), first); err != nil {
		t.Fatalf("reconcileRBAC: %v", err)
	}

	if got := recordedReasons(recorder)[eksRoleChangedReason]; got != 2 {
		t.Errorf("%s events = %d, want one on each of the two clusters", eksRoleChangedReason, got)
	}
}

// An account already carrying the stated role is no news.
func TestAnUnchangedEKSRoleWarnsNobody(t *testing.T) {
	cp := localControlPlaneSingleton()
	cp.Spec.Source.Local.EKSRoleARN = testEKSRoleARN
	first := aNamedCluster("first", firstClusterUUID, atlaskube.ConstantWorkloadNames())
	account := staleRoleAccount()
	account.Annotations[utils.EKSRoleARNAnnotation] = testEKSRoleARN
	r := aNamesReconciler(t, cp, first, account)
	recorder := events.NewFakeRecorder(8)
	r.Recorder = recorder

	if err := r.reconcileRBAC(context.Background(), first); err != nil {
		t.Fatalf("reconcileRBAC: %v", err)
	}

	if got := recordedReasons(recorder)[eksRoleChangedReason]; got != 0 {
		t.Errorf("%s events = %d, want none", eksRoleChangedReason, got)
	}
}

// Every cluster's ServiceAccount follows the singleton ControlPlane, so a
// changed role wakes every cluster, in any namespace, and an edit that leaves
// the role alone wakes none.
func TestAControlPlaneRoleChangeEnqueuesEveryCluster(t *testing.T) {
	first := aNamedCluster("first", firstClusterUUID, atlaskube.ConstantWorkloadNames())
	second := aNamedCluster(secondCluster, secondClusterUUID, atlaskube.DerivedWorkloadNames(secondCluster))
	second.Namespace = "elsewhere"
	r := aNamesReconciler(t, first, second)

	before := localControlPlaneSingleton()
	after := localControlPlaneSingleton()
	after.Spec.Source.Local.EKSRoleARN = testEKSRoleARN

	if !eksRoleChanged().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Error("a changed role is filtered out")
	}
	same := before.DeepCopy()
	same.Spec.Source.Local.Image = "docker.io/simplyblock/simplyblock:2.0"
	if eksRoleChanged().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: same}) {
		t.Error("an edit that leaves the role alone is let through")
	}
	if got := r.everyCluster(context.Background(), after); len(got) != 2 {
		t.Errorf("enqueued %v, want both clusters, in either namespace", got)
	}
}
