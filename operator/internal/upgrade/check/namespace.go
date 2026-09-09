// §19.10's fifth check, and the two of §19.8's uniqueness routes that the name
// checks cannot see.
//
// Both are the same shape. An identifier that is unique inside a namespace
// becomes ambiguous in a space that has no namespaces, and neither the objects
// nor the operator notice: the API server accepts both, and one of them
// silently takes what the other needs. A StorageCluster's name reaches the
// worker Nodes as a label carrying nothing else, so two clusters of one name in
// two namespaces claim the same machines. A VolumeMigration becomes a
// cluster-scoped PersistentVolumeOps, so two of one name in two namespaces
// become one object.
//
// Neither is visible from inside one installation, which is why this is the one
// check that reads upgrade.Scope.ClusterWide.

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

// collapses are the rows. There are two because the target model has two, and
// the list is where a third goes rather than in a new check.
func collapses() []Collapse {
	return []Collapse{
		{
			Kind: schema.GroupVersionKind{
				Group: "storage.simplyblock.io", Version: "v1alpha1", Kind: "StorageCluster",
			},
			Into:    "one storage plane",
			Because: "the io.simplyblock.node-type label a cluster claims workers with carries the cluster name and nothing else",
			Fix:     upgrade.FixBoundInput,
		},
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
// Only a group touching the installation's own namespace is reported. A
// collision between two namespaces that are both somebody else's is real, and
// it is their upgrade's problem rather than this one's: blocking here would
// make an installation's preflight fail on a cluster it does not own.
func (c Collapse) check(s *upgrade.Scope) upgrade.Findings {
	key := c.Key
	if key == nil {
		key = func(obj client.Object) string { return obj.GetName() }
	}

	byKey := make(map[string][]client.Object)
	for _, obj := range s.ClusterWide.OfKind(c.Kind) {
		byKey[key(obj)] = append(byKey[key(obj)], obj)
	}

	shared := make([]string, 0, len(byKey))
	for value, objects := range byKey {
		if len(objects) > 1 && touchesInstallation(objects, s.Namespace) {
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

// touchesInstallation reports whether any of the colliding objects is the one
// this run is upgrading.
func touchesInstallation(objects []client.Object, namespace string) bool {
	for _, obj := range objects {
		if obj.GetNamespace() == namespace {
			return true
		}
	}
	return false
}

// finding renders one collapse.
func (c Collapse) finding(s *upgrade.Scope, value string, objects []client.Object) upgrade.Finding {
	sorted := append([]client.Object(nil), objects...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetNamespace() < sorted[j].GetNamespace() })

	refs := make([]upgrade.ObjectRef, 0, len(sorted))
	notes := make([]string, 0, len(sorted))
	for _, obj := range sorted {
		refs = append(refs, s.Ref(obj))
		if obj.GetNamespace() == s.Namespace {
			notes = append(notes, "this installation")
			continue
		}
		notes = append(notes, "another installation")
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
