// The RBAC the two plugins need: a ServiceAccount each, and the five ClusterRole
// and ClusterRoleBinding pairs behind them.
//
// The rules are the ones the chart applies, because adoption reconciles toward
// the state that is running and a rule this file widens or narrows is a
// permission change nobody asked for. Each sidecar gets its own role rather than
// one role for the controller plugin, so a cluster running a second CSI driver
// runs a second set of its own and each set says which sidecar needs what.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §4.1 and §4.3.

package driver

import (
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// rule is the shorthand the tables below are written in.
func rule(groups []string, resources []string, verbs ...string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{APIGroups: groups, Resources: resources, Verbs: verbs}
}

var (
	core     = []string{""}
	storage  = []string{"storage.k8s.io"}
	snapshot = []string{"snapshot.storage.k8s.io"}
)

// clusterRoleRules is the rule set of each of the five roles, keyed by the
// component in its name.
var clusterRoleRules = map[string][]rbacv1.PolicyRule{
	"node": {
		rule(core, []string{"nodes"}, "get", "list", "watch"),
		rule(core, []string{"pods"}, "get", "list", "watch", "delete"),
		rule(core, []string{"persistentvolumeclaims"}, "get", "list", "watch", "patch"),
		rule(core, []string{"persistentvolumes"}, "get", "list", "watch"),
		rule(storage, []string{"storageclasses"}, "get", "list", "watch"),
		rule(core, []string{"events"}, "create", "patch"),
	},
	"provisioner": {
		rule(core, []string{"secrets"}, "get", "list"),
		rule(core, []string{"persistentvolumes"}, "get", "list", "watch", "create", "delete", "patch"),
		rule(core, []string{"persistentvolumeclaims"}, "get", "list", "watch", "update", "patch"),
		rule(storage, []string{"storageclasses"}, "get", "list", "watch"),
		rule(core, []string{"events"}, "list", "watch", "create", "update", "patch"),
		rule(snapshot, []string{"volumesnapshots"}, "get", "list"),
		rule(snapshot, []string{"volumesnapshotcontents"}, "create", "get", "list", "watch", "update", "delete", "patch"),
		rule(snapshot, []string{"volumesnapshotclasses"}, "get", "list", "watch"),
		rule(snapshot, []string{"volumesnapshotcontents/status"}, "get", "update", "patch"),
		rule(storage, []string{"csinodes"}, "get", "list", "watch"),
		rule(core, []string{"nodes"}, "get", "list", "watch"),
		rule(storage, []string{"volumeattachments"}, "get", "list", "watch"),
	},
	"attacher": {
		rule(core, []string{"persistentvolumes"}, "get", "list", "watch", "update", "patch"),
		rule(storage, []string{"csinodes"}, "get", "list", "watch"),
		rule(storage, []string{"volumeattachments"}, "get", "list", "watch", "update", "patch"),
		rule(storage, []string{"volumeattachments/status"}, "patch"),
		rule(core, []string{"secrets"}, "get", "list"),
	},
	"resizer": {
		rule(core, []string{"persistentvolumes"}, "get", "list", "watch", "update", "patch"),
		rule(core, []string{"persistentvolumeclaims"}, "get", "list", "watch", "update", "patch"),
		rule(core, []string{"persistentvolumeclaims/status"}, "patch"),
		rule(storage, []string{"storageclasses"}, "get", "list", "watch"),
		rule(core, []string{"events"}, "list", "watch", "create", "update", "patch"),
		rule(core, []string{"pods"}, "get", "list", "watch", "update"),
	},
	"health-monitor": {
		rule(core, []string{"persistentvolumes"}, "get", "list", "watch"),
		rule(core, []string{"persistentvolumeclaims"}, "get", "list", "watch"),
		rule(core, []string{"nodes"}, "get", "list", "watch"),
		rule(core, []string{"pods"}, "get", "list", "watch"),
		rule(core, []string{"events"}, "get", "list", "watch", "create", "patch"),
	},
}

// serviceAccountFor names the account each role is bound to. The node plugin has
// its own, and the controller plugin's sidecars share one.
func serviceAccountFor(n objectNames, component string) string {
	if component == "node" {
		return n.nodeServiceAccount
	}
	return n.controllerServiceAccount
}

func serviceAccounts(d *simplyblockv1alpha2.SimplyblockDriver) []*corev1.ServiceAccount {
	n := names(d)
	out := make([]*corev1.ServiceAccount, 0, 2)
	for _, name := range []string{n.nodeServiceAccount, n.controllerServiceAccount} {
		out = append(out, &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: d.Namespace},
		})
	}
	return out
}

func clusterRoles(d *simplyblockv1alpha2.SimplyblockDriver) []*rbacv1.ClusterRole {
	n := names(d)
	out := make([]*rbacv1.ClusterRole, 0, len(clusterRoleComponents))
	for _, component := range clusterRoleComponents {
		out = append(out, &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: n.clusterRole(component)},
			Rules:      clusterRoleRules[component],
		})
	}
	return out
}

func clusterRoleBindings(d *simplyblockv1alpha2.SimplyblockDriver) []*rbacv1.ClusterRoleBinding {
	n := names(d)
	out := make([]*rbacv1.ClusterRoleBinding, 0, len(clusterRoleComponents))
	for _, component := range clusterRoleComponents {
		out = append(out, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: n.clusterRoleBinding(component)},
			Subjects: []rbacv1.Subject{{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      serviceAccountFor(n, component),
				Namespace: d.Namespace,
			}},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     n.clusterRole(component),
			},
		})
	}
	return out
}
