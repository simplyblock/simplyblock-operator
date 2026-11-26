// Whether serviceAccountName carries the EKS role annotation a local control
// plane's spec.source.local.eksRoleARN states, that the FoundationDB
// accounts, which never reach S3, never do, and that a changed role rolls
// the workloads running as the account.

package controlplane

import (
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
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
		sa := findServiceAccount(t, foundationDBObjects(cp), name)
		if _, ok := sa.Annotations[utils.EKSRoleARNAnnotation]; ok {
			t.Errorf("%s annotations = %v, want no EKS role annotation", name, sa.Annotations)
		}
	}
}

// accountWorkloads is every Deployment whose pods run as serviceAccountName,
// the account the EKS role annotation is on.
func accountWorkloads(t *testing.T, objects []client.Object) []*appsv1.Deployment {
	t.Helper()
	var out []*appsv1.Deployment
	for _, obj := range objects {
		if d, ok := obj.(*appsv1.Deployment); ok && d.Spec.Template.Spec.ServiceAccountName == serviceAccountName {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		t.Fatal("no Deployment runs as the management API's account")
	}
	return out
}

// The pod-identity webhook injects the role when a pod is admitted, so a
// changed eksRoleARN reaches running pods only through a new pod template:
// each workload on the account carries the role on its template.
func TestAChangedEKSRoleRollsTheAccountsWorkloads(t *testing.T) {
	before := localControlPlane()
	before.Spec.Source.Local.EKSRoleARN = testEKSRoleARN
	after := localControlPlane()
	after.Spec.Source.Local.EKSRoleARN = "arn:aws:iam::123456789012:role/simplyblock-rotated"
	unset := localControlPlane()

	old := map[string]map[string]string{}
	for _, d := range accountWorkloads(t, managementAPIObjects(before)) {
		old[d.Name] = d.Spec.Template.Annotations
	}
	for _, d := range accountWorkloads(t, managementAPIObjects(after)) {
		if reflect.DeepEqual(old[d.Name], d.Spec.Template.Annotations) {
			t.Errorf("%s: the pod template is unchanged by a new role, so its pods keep the old one", d.Name)
		}
	}
	for _, d := range accountWorkloads(t, managementAPIObjects(unset)) {
		if reflect.DeepEqual(old[d.Name], d.Spec.Template.Annotations) {
			t.Errorf("%s: the pod template is unchanged by removing the role", d.Name)
		}
	}
}
