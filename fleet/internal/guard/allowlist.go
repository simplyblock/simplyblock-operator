// The identities a member's storage group may be written by.
//
// The list is data rather than part of the rule, because the identities are not
// knowable when the rule is written. The operator's own service account has one
// spelling under the Helm chart and another under Kustomize and OLM, and the
// namespace it runs in is chosen by whoever installed it, so a member's list is
// composed for that member and delivered beside the policy.
//
// Three of the entries are not about people at all. The work agent is the
// identity every payload from the hub lands under, and the garbage collector and
// the namespace controller are how most objects of this group are deleted, so a
// list that omits any of them stops the fleet rather than stopping a person.

package guard

import (
	"fmt"
	"strings"
)

// separator joins the rendered entries. The policy splits on it and trims each
// entry, so a rendered list stays readable as one entry per line, and an entry
// that carries the separator itself is refused by Validate rather than silently
// becoming two.
const separator = ","

// The identities that are the same in every member, because Open Cluster
// Management and Kubernetes choose them rather than an installer.
const (
	// WorkAgentUser applies every payload the hub delivers.
	WorkAgentUser = "system:serviceaccount:open-cluster-management-agent:klusterlet-work-sa"
	// GarbageCollectorUser deletes by cascade down the ownership spine.
	GarbageCollectorUser = "system:serviceaccount:kube-system:generic-garbage-collector"
	// NamespaceControllerUser removes a terminating namespace's objects.
	NamespaceControllerUser = "system:serviceaccount:kube-system:namespace-controller"

	// AddOnServiceAccountGroup covers every add-on agent by its namespace, so
	// that an add-on gaining a service account does not need the list edited.
	AddOnServiceAccountGroup = "system:serviceaccounts:open-cluster-management-agent-addon"
	// BreakGlassGroup has no members until somebody binds it. It exists so that
	// an incident on a member is resolved by an auditable grant rather than by
	// deleting the policy.
	BreakGlassGroup = "simplyblock:break-glass"
)

// Allowlist is the set of identities the guard admits. Users are matched whole,
// and groups are matched against the groups the API server asserts for the
// requester.
type Allowlist struct {
	// Users are usernames, matched exactly and never by prefix.
	Users []string

	// Groups are group names. One membership is enough.
	Groups []string
}

// DefaultAllowlist is what a member receives before anything is known about it.
//
// It names all three spellings of the operator's service account, because a
// member runs one of them and the other two exist nowhere, so naming all three
// costs nothing and removes a lookup the hub cannot perform without reading the
// member.
func DefaultAllowlist() Allowlist {
	return Allowlist{
		Users: []string{
			"system:serviceaccount:simplyblock:simplyblock-operator",
			"system:serviceaccount:simplyblock-system:simplyblock-operator",
			"system:serviceaccount:simplyblock-operator-system:simplyblock-operator-controller-manager",
			WorkAgentUser,
			GarbageCollectorUser,
			NamespaceControllerUser,
		},
		Groups: []string{
			AddOnServiceAccountGroup,
			BreakGlassGroup,
		},
	}
}

// WithOperator returns a copy naming the operator's service account in one
// member. It is what a member whose operator runs somewhere the default does not
// name is composed with, and it is idempotent, so composing twice from a
// reconcile that ran twice yields the same list.
func (a Allowlist) WithOperator(namespace, serviceAccount string) Allowlist {
	return a.withUser(fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccount))
}

func (a Allowlist) withUser(username string) Allowlist {
	out := Allowlist{
		Users:  append([]string(nil), a.Users...),
		Groups: append([]string(nil), a.Groups...),
	}
	for _, existing := range out.Users {
		if existing == username {
			return out
		}
	}
	out.Users = append(out.Users, username)
	return out
}

// RenderUsers is the form the policy's parameter carries. Empty and
// whitespace-only entries are dropped, because an empty entry in the rendered
// list matches an empty username and an unauthenticated request carries one.
func (a Allowlist) RenderUsers() string { return render(a.Users) }

// RenderGroups is RenderUsers for the group list.
func (a Allowlist) RenderGroups() string { return render(a.Groups) }

func render(entries []string) string {
	kept := make([]string, 0, len(entries))
	for _, entry := range entries {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, separator+"\n")
}

// Validate refuses a list that would not mean what it reads as. An entry
// carrying the separator becomes two entries once the policy splits it, which is
// how an identity nobody reviewed reaches an allowlist somebody did.
func (a Allowlist) Validate() error {
	for _, group := range [][]string{a.Users, a.Groups} {
		for _, entry := range group {
			if strings.Contains(entry, separator) {
				return fmt.Errorf("allowlist entry %q carries the separator %q, which the policy would read as two entries", entry, separator)
			}
		}
	}
	return nil
}
