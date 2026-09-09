// §19.10's fifth check: no kind that becomes cluster-scoped has same-named
// objects in two namespaces.
//
// It is about an object's own identity rather than about a name derived from
// one. A VolumeMigration is namespaced and the PersistentVolumeOps that absorbs
// it is not (§16.2), so two migrations of one name in two namespaces are one
// object in the target model, and the migration would copy one over the other.
// No naming rule models that, because nothing is being derived.
//
// The derived-name half of §19.8, including the node label a StorageNodeSet
// claims workers with, belongs to derived-names-unique instead. That check
// knows where each value has to be unique, so it reports a label value
// colliding across namespaces without reporting every namespaced object name
// that merely repeats.

package check

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// IDNamespaceCollapse is the check's identity.
const IDNamespaceCollapse upgrade.ID = "namespace-collapse"

// Collapse is one kind whose objects share a space with no namespace in it.
type Collapse struct {
	// Kind is what is read, out of the cluster-wide graph.
	Kind schema.GroupVersionKind

	// Into names the space the objects collapse into, in the form a report
	// prints, such as one cluster-scoped PersistentVolumeOps.
	Into string

	// Because says why the space has no namespace, which is the half a user
	// needs in order to believe the finding.
	Because string

	// Key is what two objects have to share to collide. It defaults to the
	// object's name, which is what both of today's rows want.
	Key func(client.Object) string

	// Fix is what resolves it.
	Fix upgrade.Fix
}

// collapses are the rows. There is one, because §16.2 makes one kind
// cluster-scoped, and the list is where a second goes rather than in a new
// check.
func collapses() []Collapse {
	return []Collapse{
		{
			Kind: schema.GroupVersionKind{
				Group: "storage.simplyblock.io", Version: "v1alpha1", Kind: "VolumeMigration",
			},
			Into:    "one cluster-scoped PersistentVolumeOps",
			Because: "§16.2 absorbs VolumeMigration into PersistentVolumeOps, which is cluster-scoped and has no namespace to keep them apart",
			Fix:     upgrade.FixTruncateAndHash,
		},
	}
}

// NamespaceCollapse returns the check.
func NamespaceCollapse() upgrade.Check {
	return upgrade.CheckFunc{
		RuleID:  IDNamespaceCollapse,
		Summary: "no two namespaces hold objects that become one where the namespace is dropped",
		RunIn:   everyStage,
		Fn: func(_ context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
			rows := collapses()
			s.Report.Work(len(rows))

			findings := make(upgrade.Findings, 0, len(rows))
			for _, row := range rows {
				s.Report.Item(row.Kind.Kind)
				findings = append(findings, row.check(s)...)
			}
			return findings, nil
		},
	}
}

// check reports the groups of this kind that collapse onto one identity.
//
// Every group is reported, wherever its objects are. There is no namespace this
// migration is not responsible for: the operator's cache is restricted to none
// and its RBAC is a ClusterRole, so both halves of a same-named pair are
// reconciled by the operator being upgraded.
func (c Collapse) check(s *upgrade.Scope) upgrade.Findings {
	key := c.Key
	if key == nil {
		key = func(obj client.Object) string { return obj.GetName() }
	}

	byKey := make(map[string][]client.Object)
	for _, obj := range s.Graph.OfKind(c.Kind) {
		if obj.GetNamespace() == "" {
			// A cluster-scoped object of this kind has no namespace to lose,
			// so it cannot be half of this collision.
			continue
		}
		byKey[key(obj)] = append(byKey[key(obj)], obj)
	}

	shared := make([]string, 0, len(byKey))
	for value, objects := range byKey {
		if distinctNamespaces(objects) > 1 {
			shared = append(shared, value)
		}
	}
	sort.Strings(shared)

	findings := make(upgrade.Findings, 0, len(shared))
	for _, value := range shared {
		findings = append(findings, c.finding(s, value, byKey[value]))
	}
	return findings
}

// distinctNamespaces counts the namespaces these objects are spread over. Two
// objects of one name in one namespace is impossible, so anything above one is
// the collision. Counting namespaces rather than objects is what keeps a kind
// whose key is not the object's name from reporting a pair that shares one.
func distinctNamespaces(objects []client.Object) int {
	seen := make(map[string]bool, len(objects))
	for _, obj := range objects {
		seen[obj.GetNamespace()] = true
	}
	return len(seen)
}

// finding renders one collapse.
func (c Collapse) finding(s *upgrade.Scope, value string, objects []client.Object) upgrade.Finding {
	sorted := append([]client.Object(nil), objects...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetNamespace() < sorted[j].GetNamespace() })

	refs := make([]upgrade.ObjectRef, 0, len(sorted))
	notes := make([]string, 0, len(sorted))
	for _, obj := range sorted {
		refs = append(refs, s.Ref(obj))
		notes = append(notes, "in "+obj.GetNamespace())
	}

	return upgrade.Finding{
		Rule:      IDNamespaceCollapse,
		Severity:  upgrade.SeverityError,
		Objects:   refs,
		PerObject: notes,
		Summary: fmt.Sprintf("%d %s objects named %q in different namespaces become %s",
			len(sorted), c.Kind.Kind, value, c.Into),
		Detail:      c.Because,
		Remediation: string(c.Fix),
	}
}
