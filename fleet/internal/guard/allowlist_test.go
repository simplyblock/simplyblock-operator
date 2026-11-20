// The allowlist's composition and its rendering into the form the policy reads.
//
// The rendering is tested separately from the rule because the two fail
// differently: a rule that does not compile refuses every write loudly, and a
// list that renders wrong refuses the operator quietly, which is the outage that
// looks like a storage problem.

package guard

import (
	"strings"
	"testing"
)

func TestDefaultAllowlistCarriesWhatTheFleetDependsOn(t *testing.T) {
	list := DefaultAllowlist()

	required := []string{
		WorkAgentUser,
		GarbageCollectorUser,
		NamespaceControllerUser,
	}
	for _, user := range required {
		if !contains(list.Users, user) {
			t.Errorf("the default allowlist omits %q, without which the fleet or the cluster's own controllers are refused", user)
		}
	}

	requiredGroups := []string{AddOnServiceAccountGroup, BreakGlassGroup}
	for _, group := range requiredGroups {
		if !contains(list.Groups, group) {
			t.Errorf("the default allowlist omits the group %q", group)
		}
	}
}

// TestDefaultAllowlistCarriesEveryOperatorSpelling covers the fact that decides
// this list is data rather than policy text: the operator's service account has
// one name under the chart and another under Kustomize and OLM, and a member
// runs one of them.
func TestDefaultAllowlistCarriesEveryOperatorSpelling(t *testing.T) {
	list := DefaultAllowlist()

	for _, want := range []string{
		"system:serviceaccount:simplyblock:simplyblock-operator",
		"system:serviceaccount:simplyblock-system:simplyblock-operator",
		"system:serviceaccount:simplyblock-operator-system:simplyblock-operator-controller-manager",
	} {
		if !contains(list.Users, want) {
			t.Errorf("the default allowlist omits the operator spelling %q", want)
		}
	}
}

func TestWithOperatorAddsOneIdentityAndKeepsTheRest(t *testing.T) {
	base := DefaultAllowlist()
	list := base.WithOperator("acme-storage", "simplyblock-operator")

	want := "system:serviceaccount:acme-storage:simplyblock-operator"
	if !contains(list.Users, want) {
		t.Fatalf("WithOperator did not add %q", want)
	}
	if len(list.Users) != len(base.Users)+1 {
		t.Errorf("WithOperator changed the list length by %d, want 1", len(list.Users)-len(base.Users))
	}
	if contains(base.Users, want) {
		t.Error("WithOperator mutated the receiver, so two members would share one list")
	}
}

func TestWithOperatorIsIdempotent(t *testing.T) {
	list := DefaultAllowlist().WithOperator("simplyblock", "simplyblock-operator")
	again := list.WithOperator("simplyblock", "simplyblock-operator")

	if len(again.Users) != len(list.Users) {
		t.Errorf("adding an identity the list already holds changed its length from %d to %d", len(list.Users), len(again.Users))
	}
}

// TestRenderRoundTrips pins the one property the policy's CEL depends on: what
// Render writes, split on the separator and trimmed, is the list that went in.
func TestRenderRoundTrips(t *testing.T) {
	list := DefaultAllowlist().WithOperator("acme", "op")

	for name, pair := range map[string][2]any{
		"users":  {list.Users, list.RenderUsers()},
		"groups": {list.Groups, list.RenderGroups()},
	} {
		want := pair[0].([]string)
		got := splitRendered(pair[1].(string))
		if len(got) != len(want) {
			t.Errorf("%s: rendered %d entries, want %d", name, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: entry %d rendered as %q, want %q", name, i, got[i], want[i])
			}
		}
	}
}

// TestRenderEmitsNoEmptyEntry is the boundary the guard turns on. An empty
// entry in the rendered list matches an empty username, and an unauthenticated
// request carries one.
func TestRenderEmitsNoEmptyEntry(t *testing.T) {
	list := Allowlist{Users: []string{"a", "", "  ", "b"}}

	for _, entry := range splitRendered(list.RenderUsers()) {
		if strings.TrimSpace(entry) == "" {
			t.Fatalf("RenderUsers emitted an empty entry from %v", list.Users)
		}
	}
}

func TestValidateRefusesAnEntryCarryingTheSeparator(t *testing.T) {
	list := Allowlist{Users: []string{"system:serviceaccount:a:b,system:masters"}}

	if err := list.Validate(); err == nil {
		t.Fatal("Validate accepted an entry carrying the separator, which smuggles a second identity into one")
	}
}

func TestValidateAcceptsTheDefault(t *testing.T) {
	if err := DefaultAllowlist().Validate(); err != nil {
		t.Fatalf("the default allowlist does not validate: %v", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, value := range haystack {
		if value == needle {
			return true
		}
	}
	return false
}

func splitRendered(rendered string) []string {
	out := []string{}
	for _, entry := range strings.Split(rendered, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
