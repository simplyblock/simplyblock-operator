// The guard's protection of itself.
//
// The rule that refuses a write to the storage group is worth nothing if the
// identity it refuses may delete the rule, so a second policy matches the
// guard's own objects by name.
//
// It carries no parameter, and that is the property these tests exist to hold.
// A self-protecting policy that read its identities from the allowlist would
// deadlock the moment the allowlist went missing: every matched request would be
// refused, the allowlist is matched, and nothing could put it back.

package guard

import (
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

// TestSelfPolicyCarriesNoParameter is the deadlock guard. A parameter here is
// not a preference, it is an unrecoverable member.
func TestSelfPolicyCarriesNoParameter(t *testing.T) {
	if policy := SelfPolicy(); policy.Spec.ParamKind != nil {
		t.Fatalf("the self policy declares paramKind %v, which deadlocks a member whose allowlist is deleted", policy.Spec.ParamKind)
	}
	if binding := SelfBinding(); binding.Spec.ParamRef != nil {
		t.Fatal("the self binding declares a paramRef, which deadlocks a member whose allowlist is deleted")
	}
}

// TestSelfPolicyMatchesTheAllowlist covers the object this policy exists for,
// by name, because a rule matching every ConfigMap in the member would guard far
// more than the guard.
func TestSelfPolicyMatchesTheAllowlist(t *testing.T) {
	if !matches(SelfPolicy(), "", "configmaps", Name) {
		t.Error("the self policy does not match the allowlist")
	}
}

// TestSelfPolicyClaimsNoRuleOnAdmissionConfiguration pins an absence.
//
// Kubernetes skips admission on admissionregistration.k8s.io resources to avoid
// circular dependencies, so a rule matching the guard's own policy and binding
// installs cleanly and never fires. An inert rule is worse than no rule, because
// it reads as a protection that is not there.
// TestAdmissionCannotGuardAdmissionConfiguration demonstrates the behavior.
func TestSelfPolicyClaimsNoRuleOnAdmissionConfiguration(t *testing.T) {
	for _, rule := range SelfPolicy().Spec.MatchConstraints.ResourceRules {
		if contains(rule.APIGroups, admissionregistrationv1.SchemeGroupVersion.Group) {
			t.Errorf("the self policy claims a rule on %v, which the API server never evaluates",
				rule.Resources)
		}
	}
}

// TestSelfPolicyGuardsEveryWriteToTheAllowlist covers what an escape needs.
// Create matters as much as update, because an allowlist that may be deleted and
// then created again is an allowlist anybody rewrites in two steps.
func TestSelfPolicyGuardsEveryWriteToTheAllowlist(t *testing.T) {
	policy := SelfPolicy()

	for _, rule := range policy.Spec.MatchConstraints.ResourceRules {
		if !contains(rule.Resources, "configmaps") {
			continue
		}
		for _, required := range []admissionregistrationv1.OperationType{
			admissionregistrationv1.Create,
			admissionregistrationv1.Update,
			admissionregistrationv1.Delete,
		} {
			if !hasOperation(rule, required) {
				t.Errorf("writes to the allowlist are unguarded for %s", required)
			}
		}
		return
	}
	t.Fatal("no rule matches the allowlist")
}

func TestSelfPolicyDecisions(t *testing.T) {
	cases := []struct {
		name     string
		username string
		groups   []string
		allowed  bool
		because  string
	}{
		{
			name:     "the work agent",
			username: WorkAgentUser,
			allowed:  true,
			because:  "it delivers the guard, and it is what restores a deleted allowlist",
		},
		{
			name:     "a break-glass operator",
			username: "sre@simplyblock.io",
			groups:   []string{BreakGlassGroup},
			allowed:  true,
			because:  "widening the allowlist needs one auditable lever that is not deleting the guard",
		},
		{
			name:     "an administrator with a kubeconfig",
			username: "kubernetes-admin",
			groups:   []string{"system:masters"},
			allowed:  false,
			because:  "adding oneself to the allowlist is the escape this policy exists to refuse",
		},
		{
			name:     "the operator",
			username: "system:serviceaccount:simplyblock:simplyblock-operator",
			allowed:  false,
			because:  "the operator writes storage objects and has no business editing admission policy",
		},
		{
			name:     "an add-on agent",
			username: "system:serviceaccount:open-cluster-management-agent-addon:simplyblock-fleet-agent",
			groups:   []string{AddOnServiceAccountGroup},
			allowed:  false,
			because:  "the agent reports, and the allowlist that lets add-ons write storage does not let them edit the guard",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if allowed := evaluateSelf(t, testCase.username, testCase.groups); allowed != testCase.allowed {
				t.Errorf("allowed = %v, want %v: %s", allowed, testCase.allowed, testCase.because)
			}
		})
	}
}

func evaluateSelf(t *testing.T, username string, groups []string) bool {
	t.Helper()

	policy := SelfPolicy()
	env := newEnv(t)
	activation := map[string]any{
		"request": map[string]any{"userInfo": map[string]any{"username": username, "groups": toAnySlice(groups)}},
	}

	resolved := map[string]any{}
	for _, variable := range policy.Spec.Variables {
		activation["variables"] = resolved
		out, _, err := compile(t, env, variable.Expression).Eval(activation)
		if err != nil {
			t.Fatalf("evaluating variable %q: %v", variable.Name, err)
		}
		resolved[variable.Name] = out.Value()
	}
	activation["variables"] = resolved

	if len(policy.Spec.Validations) != 1 {
		t.Fatalf("expected one validation, got %d", len(policy.Spec.Validations))
	}
	out, _, err := compile(t, env, policy.Spec.Validations[0].Expression).Eval(activation)
	if err != nil {
		t.Fatalf("evaluating the validation: %v", err)
	}
	allowed, ok := out.Value().(bool)
	if !ok {
		t.Fatalf("the validation returned %T, not a bool", out.Value())
	}
	return allowed
}

func matches(policy *admissionregistrationv1.ValidatingAdmissionPolicy, group, resource, name string) bool {
	for _, rule := range policy.Spec.MatchConstraints.ResourceRules {
		if !contains(rule.APIGroups, group) || !contains(rule.Resources, resource) {
			continue
		}
		if len(rule.ResourceNames) == 0 || contains(rule.ResourceNames, name) {
			return true
		}
	}
	return false
}

func hasOperation(rule admissionregistrationv1.NamedRuleWithOperations, want admissionregistrationv1.OperationType) bool {
	for _, operation := range rule.Operations {
		if operation == want || operation == admissionregistrationv1.OperationAll {
			return true
		}
	}
	return false
}
