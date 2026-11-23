package controlplane

import (
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// anyUIDSCCRole is the ClusterRole OpenShift ships to grant use of the
	// anyuid SCC.
	anyUIDSCCRole = "system:openshift:scc:anyuid"

	// anyUIDSCCBindingName is the RoleBinding that grants it to the control
	// plane's account.
	anyUIDSCCBindingName = "simplyblock-scc-anyuid"

	// requiredSCCAnnotation names the SCC a pod is admitted under, from
	// OpenShift 4.14 on. Earlier releases ignore it and pick by priority.
	requiredSCCAnnotation = "openshift.io/required-scc"
)

// anyUIDWorkloads are the Deployments that run the control plane's image as the
// image's own user. The image's services call sudo, which works only as that
// user and with privilege escalation allowed; OpenShift's default
// restricted-v2 SCC assigns a random UID and turns escalation off.
var anyUIDWorkloads = map[string]bool{
	ComponentWebAPI:       true,
	ComponentTasks:        true,
	ComponentMonitoring:   true,
	ComponentAdminControl: true,
}

// anyUIDSCCBinding lets the control plane's account use the anyuid SCC.
//
// rbac-justified: the control plane's services run sudo (the storage node
// monitor pings each node's management address with it), and under
// restricted-v2 every probe fails, so every node is reported unreachable. The
// binding is namespaced to one account, and creating it uses the manager's
// existing bind on ClusterRoles.
func anyUIDSCCBinding(namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: anyUIDSCCBindingName, Namespace: namespace},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: anyUIDSCCRole,
		},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: serviceAccountName, Namespace: namespace,
		}},
	}
}

// withAnyUIDSCC adds the anyuid grant to the management API's objects, ahead of
// the first workload so its pods are not refused before it exists, and asks for
// anyuid on the pods that run the control plane's image.
func withAnyUIDSCC(namespace string, objects []client.Object) []client.Object {
	out := make([]client.Object, 0, len(objects)+1)
	added := false
	for _, obj := range objects {
		d, isDeployment := obj.(*appsv1.Deployment)
		if isDeployment && !added {
			out = append(out, anyUIDSCCBinding(namespace))
			added = true
		}
		if isDeployment && anyUIDWorkloads[d.Name] {
			if d.Spec.Template.Annotations == nil {
				d.Spec.Template.Annotations = map[string]string{}
			}
			d.Spec.Template.Annotations[requiredSCCAnnotation] = "anyuid"
		}
		out = append(out, obj)
	}
	if !added {
		out = append(out, anyUIDSCCBinding(namespace))
	}
	return out
}
