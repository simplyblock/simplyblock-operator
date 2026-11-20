// The guard's protection of its allowlist.
//
// A rule that refuses a write to the storage group is worth less if the identity
// it refuses may rewrite the list of who is allowed. This second policy matches
// the allowlist by name, so adding oneself to it, and deleting it to write a new
// one, are both guarded writes.
//
// It does not, and cannot, protect the policy objects themselves. Kubernetes
// skips admission on admissionregistration.k8s.io resources to avoid circular
// dependencies, so no policy and no webhook can intercept its own deletion, and a
// sufficiently privileged user in the member can remove the guard with nothing in
// the admission chain to stop them. A rule matching those resources compiles,
// installs, and never fires, which is worse than its absence, so this policy does
// not carry one. TestAdmissionCannotGuardAdmissionConfiguration pins the
// behavior so that nobody adds one back believing it works.
//
// It carries no parameter, and that is load-bearing rather than incidental. Were
// the self-protection part of the parameterized binding, a member whose allowlist
// went missing would refuse every matched request, and the allowlist is one of
// the matched objects, so nothing could put it back. Independent of the
// parameter, the work agent can always restore a deleted allowlist, and the
// storage group stays refused in the meantime, which is the correct direction to
// fail in.
//
// Its allowed identities are hardcoded because they are the two that do not vary:
// the work agent, which is what delivers the guard into a member, and the
// break-glass group, which is the one auditable lever that is not deleting the
// policy. The operator is deliberately absent. It writes storage objects and has
// no business editing admission policy.

package guard

import (
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SelfName is the self-protecting policy's name, and its binding's.
const SelfName = "simplyblock-storage-guard-self"

// selfAllowed is the whole allowlist of the self policy, inlined into its rule
// rather than read from an object, because an object is a thing that can go
// missing and this policy is what recovers from that.
var selfAllowed = []string{WorkAgentUser}

// SelfPolicy refuses a write to the guard's allowlist.
//
// The allowlist ConfigMap is matched cluster-wide by its name rather than by its
// namespace, because a policy's match conditions apply to every one of its rules
// and the other rules are on cluster-scoped objects with no namespace to compare.
// The cost is that a ConfigMap of the same name elsewhere in the member is
// guarded too, which is a name nothing else is expected to take.
func SelfPolicy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: admissionregistrationv1.SchemeGroupVersion.String(),
			Kind:       "ValidatingAdmissionPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{Name: SelfName},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: failurePolicy(admissionregistrationv1.Fail),
			MatchConstraints: &admissionregistrationv1.MatchResources{
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
					{
						// Create as well as update and delete. An allowlist that
						// may be deleted and then created again is an allowlist
						// anybody rewrites in two steps.
						ResourceNames: []string{Name},
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{
								admissionregistrationv1.Create,
								admissionregistrationv1.Update,
								admissionregistrationv1.Delete,
							},
							Rule: admissionregistrationv1.Rule{
								APIGroups:   []string{""},
								APIVersions: []string{"v1"},
								Resources:   []string{"configmaps"},
							},
						},
					},
				},
			},
			Variables: []admissionregistrationv1.Variable{{
				Name:       "isAllowed",
				Expression: selfAllowedExpression(),
			}},
			Validations: []admissionregistrationv1.Validation{{
				Expression: "variables.isAllowed",
				MessageExpression: "'the simplyblock admission guard\\'s allowlist is the fleet\\'s to change. ' + " +
					"request.userInfo.username + ' is not the fleet'",
				Reason: validationReason(metav1.StatusReasonForbidden),
			}},
			AuditAnnotations: []admissionregistrationv1.AuditAnnotation{{
				Key:             "denied-requester",
				ValueExpression: "variables.isAllowed ? '' : request.userInfo.username",
			}},
		},
	}
}

// selfAllowedExpression is the identity check, built from the constants above so
// that the rule and the names it admits cannot drift apart.
func selfAllowedExpression() string {
	users := "["
	for i, user := range selfAllowed {
		if i > 0 {
			users += ", "
		}
		users += "'" + user + "'"
	}
	users += "]"

	return "request.userInfo.username in " + users +
		" || '" + BreakGlassGroup + "' in request.userInfo.groups"
}

// SelfBinding enforces the self policy. It takes no actions argument: an
// audit-only rollout is how the storage group's allowlist is discovered, and
// there is nothing to discover about who may delete the guard.
func SelfBinding() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: admissionregistrationv1.SchemeGroupVersion.String(),
			Kind:       "ValidatingAdmissionPolicyBinding",
		},
		ObjectMeta: metav1.ObjectMeta{Name: SelfName},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: SelfName,
			ValidationActions: []admissionregistrationv1.ValidationAction{
				admissionregistrationv1.Deny,
				admissionregistrationv1.Audit,
			},
		},
	}
}
