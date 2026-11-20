// The admission guard: in a member, the storage group is written by the fleet,
// by the operator, and by nothing else.
//
// The rule is keyed on the requester's identity, and CEL reads that from
// request.userInfo, so the whole of it fits in a ValidatingAdmissionPolicy and is
// evaluated inside the API server.
//
// What rules out a workload serving the same rule is that the guard sits in front
// of the operator's own writes: the operator writes StorageNode.spec.overrides
// and StorageCluster.spec.nodeConfigs itself, so a webhook at failurePolicy Fail
// stalls the member's storage control loop for as long as whatever serves it is
// down, and at Ignore the rule is advisory. A policy in the API server is
// reachable whenever the API server is, and it needs no certificate, no rotation,
// and no second replica.
//
// Admission never sees reads. GET, LIST, and WATCH do not reach an admission
// plugin, so this covers CREATE, UPDATE, and DELETE, and restricting who may read
// the group is RBAC's job.

package guard

import (
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The names the three objects carry. They are one name, because the binding
// names the policy and the parameter, and three spellings of one thing is how a
// binding ends up pointing at a parameter nobody delivered.
const (
	// Name is the policy's, the binding's, and the parameter's name.
	Name = "simplyblock-storage-guard"

	// ParamsNamespace is where the parameter lives in the member. It is the
	// add-on's own namespace, so the parameter is removed when the add-on is.
	ParamsNamespace = "open-cluster-management-agent-addon"

	// GuardedGroup is the API group the guard covers.
	GuardedGroup = "storage.simplyblock.io"

	// usersKey and groupsKey are the parameter's keys, and the policy reads them
	// by these names.
	usersKey  = "allowedUsers"
	groupsKey = "allowedGroups"
)

// Policy is the rule. It reads its allowlist through paramKind rather than
// carrying it, so one policy serves every member and the list is what varies.
func Policy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: admissionregistrationv1.SchemeGroupVersion.String(),
			Kind:       "ValidatingAdmissionPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{Name: Name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: failurePolicy(admissionregistrationv1.Fail),
			ParamKind: &admissionregistrationv1.ParamKind{
				APIVersion: "v1",
				Kind:       "ConfigMap",
			},
			MatchConstraints: &admissionregistrationv1.MatchResources{
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{
							admissionregistrationv1.Create,
							admissionregistrationv1.Update,
							admissionregistrationv1.Delete,
						},
						Rule: admissionregistrationv1.Rule{
							APIGroups:   []string{GuardedGroup},
							APIVersions: []string{"*"},
							// A bare "*" matches every resource of the group and
							// no subresource, which is what leaves status out.
							// Only the operator writes status, it carries no
							// intent for the fleet to protect, and it is the
							// hottest write path in the member.
							Resources: []string{"*"},
						},
					},
				}},
			},
			Variables: []admissionregistrationv1.Variable{
				{
					Name:       "allowedUsers",
					Expression: splitExpression(usersKey, "u"),
				},
				{
					Name:       "allowedGroups",
					Expression: splitExpression(groupsKey, "g"),
				},
				{
					Name: "isAllowed",
					Expression: "request.userInfo.username in variables.allowedUsers || " +
						"variables.allowedGroups.exists(g, g in request.userInfo.groups)",
				},
			},
			Validations: []admissionregistrationv1.Validation{{
				Expression: "variables.isAllowed",
				MessageExpression: "'in a fleet-managed cluster the " + GuardedGroup + " group is written by the " +
					"fleet, the operator, and nothing else. ' + request.userInfo.username + " +
					"' is not in the allowlist'",
				Reason: validationReason(metav1.StatusReasonForbidden),
			}},
			AuditAnnotations: []admissionregistrationv1.AuditAnnotation{{
				// Empty for an allowed request, which the API server omits, so
				// the annotation records only what was refused. Under an
				// audit-only binding this is how the real allowlist is
				// discovered on a live cluster before anything is enforced.
				//
				// Empty rather than null, which reads more directly and is what
				// the field documents first. A conditional null does not
				// type-check: the API server's CEL has no overload for a
				// ternary whose branches are a string and a null, so a policy
				// written that way is refused at install.
				Key:             "denied-requester",
				ValueExpression: "variables.isAllowed ? '' : request.userInfo.username",
			}},
		},
	}
}

// splitExpression reads one key of the parameter into a list. The key may be
// absent, each entry is trimmed so that a parameter written one entry per line
// reads the same as one written on a single line, and empty entries are dropped
// so that nothing in the list matches an empty username.
func splitExpression(key, variable string) string {
	return "('" + key + "' in params.data ? params.data['" + key + "'] : '')" +
		".split('" + separator + "')" +
		".map(" + variable + ", " + variable + ".trim())" +
		".filter(" + variable + ", " + variable + " != '')"
}

// Binding enforces the policy against the parameter.
//
// A missing parameter denies rather than admits. An absent allowlist is a broken
// install and not a reason to stop checking, and the parameter travels in the
// same payload as the policy, so the work agent restores a deleted one.
//
// The actions are the caller's, because a first rollout onto a member whose
// writers are not yet known carries Audit alone, reads the denied-requester
// annotations until they are empty, and only then enforces.
func Binding(actions ...admissionregistrationv1.ValidationAction) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	if len(actions) == 0 {
		actions = []admissionregistrationv1.ValidationAction{
			admissionregistrationv1.Deny,
			admissionregistrationv1.Audit,
		}
	}
	notFound := admissionregistrationv1.DenyAction
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: admissionregistrationv1.SchemeGroupVersion.String(),
			Kind:       "ValidatingAdmissionPolicyBinding",
		},
		ObjectMeta: metav1.ObjectMeta{Name: Name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: Name,
			ParamRef: &admissionregistrationv1.ParamRef{
				Name:                    Name,
				Namespace:               ParamsNamespace,
				ParameterNotFoundAction: &notFound,
			},
			ValidationActions: actions,
		},
	}
}

// Params is the allowlist as the object the policy reads.
func Params(list Allowlist) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name,
			Namespace: ParamsNamespace,
		},
		Data: map[string]string{
			usersKey:  list.RenderUsers(),
			groupsKey: list.RenderGroups(),
		},
	}
}

// Objects is the whole guard for one member, in the order it is applied.
//
// They travel together in one payload. Applied separately, a policy that lands
// before its parameter refuses every write to the group until the parameter
// follows, which is the binding's deny-on-missing behavior working exactly as
// intended against an install that split them.
//
// The last two are self.go's, and they are what make the first three more than
// advisory: without them the identity this policy refuses may delete the policy.
func Objects(list Allowlist, actions ...admissionregistrationv1.ValidationAction) []runtime.Object {
	return []runtime.Object{
		Params(list),
		Policy(),
		Binding(actions...),
		SelfPolicy(),
		SelfBinding(),
	}
}

func failurePolicy(policy admissionregistrationv1.FailurePolicyType) *admissionregistrationv1.FailurePolicyType {
	return &policy
}

func validationReason(reason metav1.StatusReason) *metav1.StatusReason {
	return &reason
}
