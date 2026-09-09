// The RBAC has to be the chart's, because adoption reconciles toward the state
// that is running: a rule this package widens is a permission the deployment did
// not have before the handover, and one it narrows is a sidecar that stops
// working after it.
//
// The subject namespace is the load-bearing part. Each binding names a
// ServiceAccount together with the namespace it lives in, which is why two
// drivers writing one binding alternate its subject, and why design §3.4 keeps a
// Kubernetes cluster to one driver.

package driver

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

func TestEveryComponentHasARoleAndABinding(t *testing.T) {
	d := testDriver("simplyblock")

	roles := clusterRoles(d)
	bindings := clusterRoleBindings(d)
	if len(roles) != len(clusterRoleComponents) || len(bindings) != len(clusterRoleComponents) {
		t.Fatalf("got %d roles and %d bindings, want %d of each",
			len(roles), len(bindings), len(clusterRoleComponents))
	}

	for _, r := range roles {
		if len(r.Rules) == 0 {
			t.Errorf("%s has no rules, so its sidecar can do nothing", r.Name)
		}
	}
}

// Each binding names its ServiceAccount in this driver's namespace, and points
// at the role of the same component.
func TestBindingsNameTheirOwnAccountAndRole(t *testing.T) {
	d := testDriver("simplyblock")
	n := names(d)

	byName := map[string]*rbacv1.ClusterRoleBinding{}
	for _, b := range clusterRoleBindings(d) {
		byName[b.Name] = b
	}

	for _, component := range clusterRoleComponents {
		b := byName[n.clusterRoleBinding(component)]
		if b == nil {
			t.Errorf("no binding for %s", component)
			continue
		}
		if len(b.Subjects) != 1 {
			t.Errorf("%s has %d subjects, want exactly this driver's", b.Name, len(b.Subjects))
			continue
		}
		s := b.Subjects[0]
		if s.Namespace != d.Namespace {
			t.Errorf("%s subject namespace = %q, want %q", b.Name, s.Namespace, d.Namespace)
		}
		want := n.controllerServiceAccount
		if component == "node" {
			want = n.nodeServiceAccount
		}
		if s.Name != want {
			t.Errorf("%s subject = %q, want %q", b.Name, s.Name, want)
		}
		if b.RoleRef.Name != n.clusterRole(component) {
			t.Errorf("%s roleRef = %q, want %q", b.Name, b.RoleRef.Name, n.clusterRole(component))
		}
	}
}

// The node plugin's account is not the controller plugin's. The node plugin runs
// privileged on every worker, and giving it the provisioner's rights would put
// volume creation behind every node in the cluster.
func TestTheTwoAccountsAreSeparate(t *testing.T) {
	d := testDriver("simplyblock")
	accounts := serviceAccounts(d)
	if len(accounts) != 2 {
		t.Fatalf("got %d service accounts, want 2", len(accounts))
	}
	if accounts[0].Name == accounts[1].Name {
		t.Errorf("both accounts are %q", accounts[0].Name)
	}
	for _, sa := range accounts {
		if sa.Namespace != d.Namespace {
			t.Errorf("%s is in %q, want %q", sa.Name, sa.Namespace, d.Namespace)
		}
	}
}

// The rules are the chart's. These are the ones a reader would most likely tidy,
// and each is load bearing: the node plugin deletes pods to recover a stuck
// mount, and the provisioner patches volumesnapshotcontents/status.
func TestRulesMatchTheChart(t *testing.T) {
	tests := []struct {
		component string
		group     string
		resource  string
		verbs     []string
	}{
		{"node", "", "pods", []string{"get", "list", "watch", "delete"}},
		{"node", "", "events", []string{"create", "patch"}},
		{"provisioner", "snapshot.storage.k8s.io", "volumesnapshotcontents/status", []string{"get", "update", "patch"}},
		{"provisioner", "", "persistentvolumes", []string{"get", "list", "watch", "create", "delete", "patch"}},
		{"attacher", "storage.k8s.io", "volumeattachments/status", []string{"patch"}},
		{"resizer", "", "persistentvolumeclaims/status", []string{"patch"}},
		{"health-monitor", "", "events", []string{"get", "list", "watch", "create", "patch"}},
	}

	for _, tc := range tests {
		t.Run(tc.component+"/"+tc.resource, func(t *testing.T) {
			var found *rbacv1.PolicyRule
			for i, r := range clusterRoleRules[tc.component] {
				if len(r.APIGroups) == 1 && r.APIGroups[0] == tc.group &&
					len(r.Resources) == 1 && r.Resources[0] == tc.resource {
					found = &clusterRoleRules[tc.component][i]
					break
				}
			}
			if found == nil {
				t.Fatalf("no rule for %s in the %s role", tc.resource, tc.component)
			}
			if len(found.Verbs) != len(tc.verbs) {
				t.Fatalf("verbs = %v, want %v", found.Verbs, tc.verbs)
			}
			for i, v := range tc.verbs {
				if found.Verbs[i] != v {
					t.Fatalf("verbs = %v, want %v", found.Verbs, tc.verbs)
				}
			}
		})
	}
}

// No rule reaches everything. A wildcard in a CSI sidecar's role is the escalation
// primitive that turns a sidecar compromise into cluster-admin.
func TestNoRuleIsAWildcard(t *testing.T) {
	for component, rules := range clusterRoleRules {
		for _, r := range rules {
			for _, v := range r.Verbs {
				if v == "*" {
					t.Errorf("%s grants a wildcard verb on %v", component, r.Resources)
				}
			}
			for _, res := range r.Resources {
				if res == "*" {
					t.Errorf("%s grants %v on every resource", component, r.Verbs)
				}
			}
		}
	}
}
