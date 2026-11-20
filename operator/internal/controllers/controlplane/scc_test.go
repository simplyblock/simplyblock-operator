// The tests of the OpenShift SCC grant: the control plane's account may use the
// anyuid SCC, and the pods that run the control plane's image ask for it.

package controlplane

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The literals are deliberate: they are the names OpenShift defines.
const (
	anyUIDClusterRole   = "system:openshift:scc:anyuid"
	requiredSCCAnnotate = "openshift.io/required-scc"
	clusterRoleKind     = "ClusterRole"
)

// controlPlaneImageWorkloads are the Deployments that run the control plane's
// image as its own user, and so reach sudo.
var controlPlaneImageWorkloads = []string{
	ComponentWebAPI, ComponentTasks, ComponentMonitoring, ComponentAdminControl,
}

func stampedObjects(t *testing.T, openShift bool) []client.Object {
	t.Helper()
	cp := localControlPlane()
	r := &ControlPlaneReconciler{Client: newClient(t, cp), Scheme: testScheme(t), OpenShift: openShift}
	objects, err := r.managementAPIStamped(context.Background(), cp)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return objects
}

// anyUIDBindings is every RoleBinding among objects that grants the anyuid SCC,
// with its position in the apply order.
func anyUIDBindings(objects []client.Object) map[int]*rbacv1.RoleBinding {
	found := map[int]*rbacv1.RoleBinding{}
	for i, obj := range objects {
		if b, ok := obj.(*rbacv1.RoleBinding); ok && b.RoleRef.Name == anyUIDClusterRole {
			found[i] = b
		}
	}
	return found
}

// Regression (OpenShift Helm install, 2026-11-20): under restricted-v2 the monitor
// runs as a random UID with privilege escalation off, its `sudo ping` fails,
// and every storage node is set unreachable. The operator grants anyuid to the
// control plane's account itself, so no manual SCC step is needed.
func TestOnOpenShiftTheControlPlanesAccountMayUseAnyUID(t *testing.T) {
	objects := stampedObjects(t, true)

	bindings := anyUIDBindings(objects)
	if len(bindings) != 1 {
		t.Fatalf("found %d RoleBindings to %s, want one", len(bindings), anyUIDClusterRole)
	}
	firstDeployment := len(objects)
	for i, obj := range objects {
		if _, ok := obj.(*appsv1.Deployment); ok {
			firstDeployment = i
			break
		}
	}
	for at, b := range bindings {
		if b.Namespace != testNamespace {
			t.Errorf("binding in namespace %q, want the control plane's %q", b.Namespace, testNamespace)
		}
		if b.RoleRef.Kind != clusterRoleKind || b.RoleRef.APIGroup != rbacv1.GroupName {
			t.Errorf("binding refers to %s/%s, want the ClusterRole", b.RoleRef.APIGroup, b.RoleRef.Kind)
		}
		if len(b.Subjects) != 1 || b.Subjects[0].Kind != rbacv1.ServiceAccountKind ||
			b.Subjects[0].Name != serviceAccountName || b.Subjects[0].Namespace != testNamespace {
			t.Errorf("binding grants %+v, want the control plane's account alone", b.Subjects)
		}
		if at > firstDeployment {
			t.Errorf("binding applied after the first Deployment: its pods would be refused before it exists")
		}
	}
}

func TestOnOpenShiftTheControlPlaneImagePodsAskForAnyUID(t *testing.T) {
	objects := stampedObjects(t, true)

	for _, name := range controlPlaneImageWorkloads {
		d := findDeployment(t, objects, name)
		if got := d.Spec.Template.Annotations[requiredSCCAnnotate]; got != "anyuid" {
			t.Errorf("%s asks for SCC %q, want anyuid", name, got)
		}
	}
}

func TestOffOpenShiftNothingNamesAnSCC(t *testing.T) {
	objects := stampedObjects(t, false)

	if n := len(anyUIDBindings(objects)); n != 0 {
		t.Errorf("found %d RoleBindings to %s off OpenShift, want none", n, anyUIDClusterRole)
	}
	for _, obj := range objects {
		d, ok := obj.(*appsv1.Deployment)
		if !ok {
			continue
		}
		if _, set := d.Spec.Template.Annotations[requiredSCCAnnotate]; set {
			t.Errorf("%s asks for an SCC off OpenShift", d.Name)
		}
	}
}
