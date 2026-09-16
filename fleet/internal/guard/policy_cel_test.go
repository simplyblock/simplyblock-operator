// The guard's rule, evaluated against the allowlist it ships with.
//
// The expressions come out of the policy the builder produces rather than out of
// a copy written here, because a CEL expression restated in a test is a test of
// the restatement. The evaluator is cel-go with the strings extension, which is
// the library Kubernetes puts in an admission policy's environment. It proves the
// rule and not the wiring around it: whether the policy binds, matches the right
// resources, and is reached at all is policy_envtest_test.go's question.

package guard

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

// newEnv builds the evaluation environment. The three names are what an
// admission policy is given, declared dynamically because this test supplies
// them rather than the API server's schemas.
func newEnv(t *testing.T) *cel.Env {
	t.Helper()

	env, err := cel.NewEnv(
		ext.Strings(),
		cel.Variable("params", cel.DynType),
		cel.Variable("request", cel.DynType),
		cel.Variable("variables", cel.DynType),
	)
	if err != nil {
		t.Fatalf("building the CEL environment: %v", err)
	}
	return env
}

// compile turns one expression into a program, failing with the expression in
// the message, because a CEL error without its source is unreadable.
func compile(t *testing.T, env *cel.Env, expression string) cel.Program {
	t.Helper()

	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compiling %q: %v", expression, issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatalf("building a program for %q: %v", expression, err)
	}
	return program
}

// TestGuardExpressionsCompile covers every expression the policy carries, not
// only the ones the decision table reaches.
//
// It is a syntax check and not an install check. The environment here declares
// its inputs dynamically, so it accepts expressions the API server's own
// type-checker refuses, and an expression that passes here can still make the
// policy fail to install. policy_envtest_test.go is what proves the policy
// installs at all.
func TestGuardExpressionsCompile(t *testing.T) {
	policy := Policy()
	env := newEnv(t)

	if len(policy.Spec.Variables) == 0 {
		t.Fatal("the policy declares no variables, so the decision table below would prove nothing")
	}
	for _, variable := range policy.Spec.Variables {
		compile(t, env, variable.Expression)
	}
	for _, validation := range policy.Spec.Validations {
		compile(t, env, validation.Expression)
		if validation.MessageExpression != "" {
			compile(t, env, validation.MessageExpression)
		}
	}
	for _, annotation := range policy.Spec.AuditAnnotations {
		compile(t, env, annotation.ValueExpression)
	}
}

// evaluate runs the policy's variables in order and then its one validation,
// which is what the API server does, and returns whether the request is allowed.
func evaluate(t *testing.T, list Allowlist, username string, groups []string) bool {
	t.Helper()

	policy := Policy()
	env := newEnv(t)
	params := Params(list)

	activation := map[string]any{
		"params":  map[string]any{"data": toAny(params.Data)},
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
		t.Fatalf("expected the guard to carry one validation, got %d", len(policy.Spec.Validations))
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

func toAny(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, value := range in {
		out = append(out, value)
	}
	return out
}

// TestGuardDecisions is the decision table. Each denial is paired with the
// allowance it would otherwise be confused with, because a rule that denies
// everything passes a test that only checks denials.
func TestGuardDecisions(t *testing.T) {
	list := DefaultAllowlist()

	cases := []struct {
		name     string
		username string
		groups   []string
		allowed  bool
		because  string
	}{
		{
			name:     "the operator installed by the chart",
			username: "system:serviceaccount:simplyblock:simplyblock-operator",
			allowed:  true,
			because:  "the operator writes its own specs, including a node's overrides",
		},
		{
			name:     "the operator installed by Kustomize or OLM",
			username: "system:serviceaccount:simplyblock-operator-system:simplyblock-operator-controller-manager",
			allowed:  true,
			because:  "the name prefix is the install path's, and both paths are supported",
		},
		{
			name:     "the work agent",
			username: WorkAgentUser,
			allowed:  true,
			because:  "every payload from the hub lands under this identity and not the fleet agent's",
		},
		{
			name:     "the garbage collector",
			username: GarbageCollectorUser,
			allowed:  true,
			because:  "most objects in this group are deleted by cascade down the ownership spine",
		},
		{
			name:     "the namespace controller",
			username: NamespaceControllerUser,
			allowed:  true,
			because:  "a deleted namespace removes its objects the same way",
		},
		{
			name:     "an add-on agent",
			username: "system:serviceaccount:open-cluster-management-agent-addon:simplyblock-fleet-agent",
			groups:   []string{AddOnServiceAccountGroup},
			allowed:  true,
			because:  "add-ons are allowed by their namespace's group rather than one name at a time",
		},
		{
			name:     "a break-glass operator",
			username: "sre@simplyblock.io",
			groups:   []string{BreakGlassGroup},
			allowed:  true,
			because:  "an incident is resolved by an auditable grant rather than by deleting the policy",
		},
		{
			name:     "an administrator with a kubeconfig",
			username: "kubernetes-admin",
			groups:   []string{"system:masters", "system:authenticated"},
			allowed:  false,
			because:  "cluster-admin is exactly the identity the guard exists to refuse",
		},
		{
			name:     "a service account in an unrelated namespace",
			username: "system:serviceaccount:default:builder",
			groups:   []string{"system:serviceaccounts:default"},
			allowed:  false,
			because:  "a namespace group match must not generalize past the add-on namespace",
		},
		{
			name:     "an add-on agent named without its group",
			username: "system:serviceaccount:open-cluster-management-agent-addon:impostor",
			allowed:  false,
			because:  "the username alone is not the claim, the group membership the API server asserts is",
		},
		{
			name:     "an anonymous request carrying no username",
			username: "",
			groups:   []string{"system:unauthenticated"},
			allowed:  false,
			because:  "an empty entry in the rendered allowlist would match this, and none is emitted",
		},
		{
			name:     "a username that is a prefix of an allowed one",
			username: "system:serviceaccount:simplyblock:simplyblock-oper",
			allowed:  false,
			because:  "the match is on the whole entry rather than on a prefix",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if allowed := evaluate(t, list, testCase.username, testCase.groups); allowed != testCase.allowed {
				t.Errorf("allowed = %v, want %v: %s", allowed, testCase.allowed, testCase.because)
			}
		})
	}
}

// TestGuardAllowsAMembersOwnOperator covers the composed case: an operator in a
// namespace the default list does not name is allowed once the member's
// allowlist names it, and is refused before that.
func TestGuardAllowsAMembersOwnOperator(t *testing.T) {
	const username = "system:serviceaccount:acme-storage:simplyblock-operator"

	if evaluate(t, DefaultAllowlist(), username, nil) {
		t.Error("the default allowlist allowed an operator namespace it does not name")
	}
	if !evaluate(t, DefaultAllowlist().WithOperator("acme-storage", "simplyblock-operator"), username, nil) {
		t.Error("the composed allowlist refused the member's own operator")
	}
}

// TestGuardMissingAllowlistKeysDenyRatherThanError is the boundary behind
// parameterNotFoundAction: a parameter object present but empty must evaluate to
// a refusal rather than to an evaluation error, because an error is a different
// failure path with different operator-facing behavior.
func TestGuardMissingAllowlistKeysDenyRatherThanError(t *testing.T) {
	if evaluate(t, Allowlist{}, WorkAgentUser, nil) {
		t.Error("an empty allowlist allowed a write")
	}
}
