// Whether the storage-node ServiceAccount carries the EKS role annotation the
// ControlPlane singleton states: added on creation, kept in sync on an
// existing account, and removed again if the role is unset, without
// disturbing any other annotation.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
