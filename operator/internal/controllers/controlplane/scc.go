// The removal of the anyuid SCC grant an earlier operator gave the control
// plane's account on OpenShift. It lives beside the install because it runs
// with the re-apply.

package controlplane

import (
	"context"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// anyUIDSCCBindingName is the RoleBinding through which an earlier operator
// granted the anyuid SCC, so that the image's `sudo ping` worked. The image now
// pings without sudo (pingSecurityContext).
const anyUIDSCCBindingName = "simplyblock-scc-anyuid"

// removeAnyUIDSCCBinding deletes the leftover grant. The ControlPlane owns it,
// so it is not garbage-collected while the ControlPlane exists. It is deleted
// by name, without a read, so no RoleBinding informer is started for it.
func (r *ControlPlaneReconciler) removeAnyUIDSCCBinding(ctx context.Context, namespace string) error {
	if !r.OpenShift {
		return nil
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: anyUIDSCCBindingName, Namespace: namespace},
	}
	return client.IgnoreNotFound(r.Delete(ctx, binding))
}
