// The RBAC the two plugins need: a ServiceAccount each, the five ClusterRole and
// ClusterRoleBinding pairs behind the sidecars that watch cluster-scoped kinds,
// and one namespaced Role and RoleBinding pair for the csi-addons sidecar, whose
// CSIAddonsNode is namespaced.
//
// The rules are the ones the chart applies, because adoption reconciles toward
// the state that is running and a rule this file widens or narrows is a
// permission change nobody asked for. Each sidecar gets its own role rather than
// one role for the controller plugin, so a cluster running a second CSI driver
// runs a second set of its own and each set says which sidecar needs what.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §4.1 and §4.3, and operator/docs/designs/design-csi-addons-replication.md §4.1.

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
	core          = []string{""}
	storage       = []string{"storage.k8s.io"}
	snapshot      = []string{"snapshot.storage.k8s.io"}
	groupsnapshot = []string{"groupsnapshot.storage.k8s.io"}
	csiaddons     = []string{"csiaddons.openshift.io"}
	coordination  = []string{"coordination.k8s.io"}
)

// clusterRoleRules is the rule set of each of the five roles, keyed by the
// component in its name.
var clusterRoleRules = map[string][]rbacv1.PolicyRule{
	nodeComponent: {
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
		// VolumeGroupSnapshot support (design-consistency-groups.md §9, P0-4):
		// the csi-snapshotter sidecar watches VolumeGroupSnapshotContent and
		// drives the GroupController behind the CSIVolumeGroupSnapshot gate.
		rule(groupsnapshot, []string{"volumegroupsnapshotclasses"}, "get", "list", "watch"),
		rule(groupsnapshot, []string{"volumegroupsnapshotcontents"}, "create", "get", "list", "watch", "update", "delete", "patch"),
		rule(groupsnapshot, []string{"volumegroupsnapshotcontents/status"}, "get", "update", "patch"),
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

// csiAddonsRoleRules is the csi-addons sidecar's rule set, granted as a
// namespaced Role rather than added to clusterRoleRules: the sidecar only ever
// touches its own CSIAddonsNode and its own leader-election Lease, both in
// this deployment's namespace, and neither kind justifies a cluster-wide grant.
var csiAddonsRoleRules = []rbacv1.PolicyRule{
	// rbac-justified: the sidecar publishes and maintains exactly one
	// CSIAddonsNode, naming itself, so the kubernetes-csi-addons
	// controller-manager (design-csi-addons-replication.md §4.1) can find its
	// endpoint. It does not read any other driver's CSIAddonsNode.
	rule(csiaddons, []string{"csiaddonsnodes"}, "get", "list", "watch", "create", "update", "delete"),
	rule(csiaddons, []string{"csiaddonsnodes/status"}, "get", "update", "patch"),
	// rbac-justified: only one replica of the controller StatefulSet serves
	// CONTROLLER_SERVICE requests at a time; the Lease is how the sidecar
	// replicas elect that one, in this namespace only.
	rule(coordination, []string{"leases"}, "get", "list", "watch", "create", "update", "delete"),
	rule(core, []string{"events"}, "create", "patch"),
}

func csiAddonsRole(d *simplyblockv1alpha2.SimplyblockDriver) *rbacv1.Role {
	n := names(d)
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: n.role(csiAddonsComponent), Namespace: d.Namespace},
		Rules:      csiAddonsRoleRules,
	}
}

func csiAddonsRoleBinding(d *simplyblockv1alpha2.SimplyblockDriver) *rbacv1.RoleBinding {
	n := names(d)
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: n.roleBinding(csiAddonsComponent), Namespace: d.Namespace},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      n.controllerServiceAccount,
			Namespace: d.Namespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     n.role(csiAddonsComponent),
		},
	}
}

// authDelegatorClusterRole is the well-known, built-in ClusterRole every
// component that validates bearer tokens via TokenReview binds to, rather
// than each defining its own copy of the same two-verb rule.
const authDelegatorClusterRole = "system:auth-delegator"

// csiAddonsAuthDelegatorBinding grants the controller plugin's account
// tokenreviews.authentication.k8s.io:create, cluster-scoped since TokenReview
// has no namespaced form. Required, not optional: the csi-addons sidecar's
// gRPC server authenticates every incoming call from the controller-manager
// by reviewing its bearer token (internal/kubernetes/token/grpc.go,
// --enable-auth defaults to true) — confirmed against a live cluster, where
// omitting this left every connection attempt failing with "failed to
// review token ... is forbidden ... at the cluster scope". Binding to the
// built-in role rather than a hand-rolled ClusterRole needs no new marker on
// the operator's own ClusterRole: the operator already holds `bind` on
// every ClusterRole unconditionally (rbac.go's clusterroles;clusterrolebindings
// marker), which is what Kubernetes' escalation prevention checks for
// referencing an existing role instead of granting its permissions directly.
func csiAddonsAuthDelegatorBinding(d *simplyblockv1alpha2.SimplyblockDriver) *rbacv1.ClusterRoleBinding {
	n := names(d)
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: n.clusterRoleBinding("csi-addons-auth-delegator")},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      n.controllerServiceAccount,
			Namespace: d.Namespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     authDelegatorClusterRole,
		},
	}
}

// serviceAccountFor names the account each role is bound to. The node plugin has
// its own, and the controller plugin's sidecars share one.
func serviceAccountFor(n objectNames, component string) string {
	if component == nodeComponent {
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
