// The identity a run's probes write their reports as. The operator creates it
// with the run rather than a chart creating it beside the operator, so a run
// anywhere gets an account that exists where its Jobs do.

package deployment

import (
	"context"
	batchv1 "k8s.io/api/batch/v1"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reachedProbing is a runner whose run has created its probe Jobs.
func reachedProbing(t *testing.T) *runner {
	t.Helper()
	r := newRunner(t, discoverRun(nil), worker("worker-1"))
	r.step() // start
	r.step() // inspect
	r.step() // probe
	return r
}

// The probe's account, Role, and binding exist in the run's namespace under a
// name derived from the run, and the Job runs as that account.
func TestAProbeRunCreatesItsOwnIdentity(t *testing.T) {
	r := reachedProbing(t)
	name := "sb-nodeprobe-" + opsName
	key := client.ObjectKey{Namespace: opsNamespace, Name: name}

	var account corev1.ServiceAccount
	if err := r.client.Get(context.Background(), key, &account); err != nil {
		t.Fatalf("the probe's ServiceAccount was not created: %v", err)
	}
	var role rbacv1.Role
	if err := r.client.Get(context.Background(), key, &role); err != nil {
		t.Fatalf("the probe's Role was not created: %v", err)
	}
	var binding rbacv1.RoleBinding
	if err := r.client.Get(context.Background(), key, &binding); err != nil {
		t.Fatalf("the probe's RoleBinding was not created: %v", err)
	}

	jobs := r.jobs()
	if len(jobs) != 1 || jobs[0].Spec.Template.Spec.ServiceAccountName != name {
		t.Errorf("the probe Job runs as %q, want %q", accountOf(jobs), name)
	}
	if binding.RoleRef.Name != name || len(binding.Subjects) != 1 || binding.Subjects[0].Name != name {
		t.Errorf("the binding does not join the Role %q to the account: %+v", name, binding)
	}
}

func accountOf(jobs []batchv1.Job) string {
	if len(jobs) == 0 {
		return "no Job"
	}
	return jobs[0].Spec.Template.Spec.ServiceAccountName
}

// The Role grants the two verbs on the one kind the probe needs, and nothing
// else: a probe that could delete reports could delete another worker's.
func TestTheProbeRoleGrantsOnlyWhatTheReportNeeds(t *testing.T) {
	r := reachedProbing(t)

	var role rbacv1.Role
	key := client.ObjectKey{Namespace: opsNamespace, Name: "sb-nodeprobe-" + opsName}
	if err := r.client.Get(context.Background(), key, &role); err != nil {
		t.Fatalf("the probe's Role was not created: %v", err)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("the Role has %d rules, want exactly one", len(role.Rules))
	}
	rule := role.Rules[0]
	if len(rule.Resources) != 1 || rule.Resources[0] != "configmaps" ||
		len(rule.Verbs) != 2 || rule.Verbs[0] != "create" || rule.Verbs[1] != "update" {
		t.Errorf("the Role grants %v on %v, want create and update on configmaps",
			rule.Verbs, rule.Resources)
	}
}

// The identity is owned by the run, so deleting the run takes it away.
func TestTheProbeIdentityIsOwnedByTheRun(t *testing.T) {
	r := reachedProbing(t)
	key := client.ObjectKey{Namespace: opsNamespace, Name: "sb-nodeprobe-" + opsName}

	var account corev1.ServiceAccount
	if err := r.client.Get(context.Background(), key, &account); err != nil {
		t.Fatalf("the probe's ServiceAccount was not created: %v", err)
	}
	if len(account.OwnerReferences) != 1 || account.OwnerReferences[0].Kind != "OperatorOps" ||
		account.OwnerReferences[0].Name != opsName {
		t.Errorf("owner references = %+v, want the run", account.OwnerReferences)
	}
}

// A second pass finds the identity and leaves it, which is what lets the probe
// step run on every requeue.
func TestAProbeRunsIdentityIsCreatedOnce(t *testing.T) {
	r := reachedProbing(t)
	r.step()

	var accounts corev1.ServiceAccountList
	if err := r.client.List(context.Background(), &accounts, client.InNamespace(opsNamespace)); err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts.Items) != 1 {
		t.Errorf("%d ServiceAccounts exist after a second pass, want 1", len(accounts.Items))
	}
}
