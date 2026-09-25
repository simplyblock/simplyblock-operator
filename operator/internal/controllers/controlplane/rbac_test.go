// What the control plane is permitted to do to Kubernetes, asserted as the work
// that needs the permission rather than as the shape of the rule.
//
// These live apart from workloads_test.go because a role is reviewed against a
// call site in the control plane's own source, not against the Deployment that
// happens to carry the account.

package controlplane

import (
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// permits reports whether the objects applied for this namespace let
// serviceAccountName perform verb on resource in it, through either a namespaced
// Role or a ClusterRole, as long as a binding names the account.
func permits(objects []client.Object, namespace, verb, resource string) bool {
	clusterRoles := map[string]*rbacv1.ClusterRole{}
	roles := map[string]*rbacv1.Role{}
	for _, obj := range objects {
		switch o := obj.(type) {
		case *rbacv1.ClusterRole:
			clusterRoles[o.Name] = o
		case *rbacv1.Role:
			roles[o.Name] = o
		}
	}

	grants := func(rules []rbacv1.PolicyRule) bool {
		for _, rule := range rules {
			if slices.Contains(rule.APIGroups, "") && slices.Contains(rule.Resources, resource) &&
				slices.Contains(rule.Verbs, verb) {
				return true
			}
		}
		return false
	}

	boundToAccount := func(subjects []rbacv1.Subject) bool {
		for _, s := range subjects {
			if s.Kind == "ServiceAccount" && s.Name == serviceAccountName &&
				s.Namespace == namespace {
				return true
			}
		}
		return false
	}

	for _, obj := range objects {
		switch b := obj.(type) {
		case *rbacv1.ClusterRoleBinding:
			if boundToAccount(b.Subjects) && b.RoleRef.Kind == "ClusterRole" {
				if role, ok := clusterRoles[b.RoleRef.Name]; ok && grants(role.Rules) {
					return true
				}
			}
		case *rbacv1.RoleBinding:
			if b.Namespace != namespace || !boundToAccount(b.Subjects) {
				continue
			}
			switch b.RoleRef.Kind {
			case "Role":
				if role, ok := roles[b.RoleRef.Name]; ok && grants(role.Rules) {
					return true
				}
			case "ClusterRole":
				if role, ok := clusterRoles[b.RoleRef.Name]; ok && grants(role.Rules) {
					return true
				}
			}
		}
	}
	return false
}

// When a node add fails after SPDK has already started, the control plane
// deletes the orphaned SPDK pod straight through the API server, because the
// node agent that would otherwise do it runs on the host the add is abandoning
// and is unreachable in exactly that case. Without delete on pods the teardown
// is a 403, the pod survives holding the host's hugepages, and every later add
// on that host gets "Insufficient hugepages-2Mi" and stays Pending.
func TestTheControlPlaneMayDeleteAnOrphanedSPDKPod(t *testing.T) {
	cp := localControlPlane()

	if !permits(managementAPIObjects(cp), cp.Namespace, "delete", "pods") {
		t.Errorf("the control plane's account cannot delete a pod in %s, "+
			"so the SPDK teardown after a failed node add is forbidden", cp.Namespace)
	}
}
