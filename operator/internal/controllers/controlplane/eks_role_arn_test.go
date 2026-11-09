// Whether serviceAccountName carries the EKS role annotation a local control
// plane's spec.source.local.eksRoleARN states, and that the FoundationDB
// accounts, which never reach S3, never do.

package controlplane

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const testEKSRoleARN = "arn:aws:iam::123456789012:role/simplyblock-backup"

func findServiceAccount(t *testing.T, objects []client.Object, name string) *corev1.ServiceAccount {
	t.Helper()
	obj := findObject(objects, name)
	sa, ok := obj.(*corev1.ServiceAccount)
	if !ok {
		t.Fatalf("%s is not a ServiceAccount: %T", name, obj)
	}
	return sa
}

func TestTheManagementAPIsAccountCarriesTheEKSRole(t *testing.T) {
	cp := localControlPlane()
	cp.Spec.Source.Local.EKSRoleARN = testEKSRoleARN

	sa := findServiceAccount(t, managementAPIObjects(cp), serviceAccountName)
	if got := sa.Annotations[utils.EKSRoleARNAnnotation]; got != testEKSRoleARN {
		t.Errorf("annotation = %q, want %q", got, testEKSRoleARN)
	}
}

func TestTheManagementAPIsAccountCarriesNoEKSRoleByDefault(t *testing.T) {
	cp := localControlPlane()

	sa := findServiceAccount(t, managementAPIObjects(cp), serviceAccountName)
	if _, ok := sa.Annotations[utils.EKSRoleARNAnnotation]; ok {
		t.Errorf("annotations = %v, want no EKS role annotation", sa.Annotations)
	}
}

// The FoundationDB accounts never reach S3, so a control plane's EKS role is
// not theirs to carry.
func TestTheFoundationDBAccountsCarryNoEKSRole(t *testing.T) {
	cp := localControlPlane()
	cp.Spec.Source.Local.EKSRoleARN = testEKSRoleARN

	for _, name := range []string{fdbOperatorServiceAccount, fdbPodServiceAccount} {
		sa := findServiceAccount(t, foundationDBObjects(cp, "", false), name)
		if _, ok := sa.Annotations[utils.EKSRoleARNAnnotation]; ok {
			t.Errorf("%s annotations = %v, want no EKS role annotation", name, sa.Annotations)
		}
	}
}
