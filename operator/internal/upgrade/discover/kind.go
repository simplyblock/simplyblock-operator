// The one discoverer every kind uses. It is a value rather than a function per
// kind because what differs between listing StorageNodes and listing
// StoragePools is a list type and a name, and eighteen copies of the same loop
// is eighteen places for a kind to be listed in the wrong namespace.

package discover

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// Kind discovers every object of one kind.
type Kind struct {
	// RuleID names the discoverer in a report and on the command line.
	RuleID upgrade.ID

	// Summary says what it reads.
	Summary string

	// List is the list type to read into. It is a prototype: each run gets a
	// deep copy, so one declaration is safe to reuse across runs and across
	// tests.
	List client.ObjectList

	// Where says which namespaces to list in. It defaults to [Everywhere],
	// which is what the group's own kinds and the cluster-scoped kinds want.
	Where Reach

	// Labels narrows a cluster-wide list to what this installation owns. It is
	// how the worker Nodes are found without reading every Node in the cluster.
	Labels map[string]string

	// Needs names the discoverers whose objects this one reads.
	Needs []upgrade.ID
}

// Reach is which namespaces a discoverer lists in.
type Reach int

const (
	// Everywhere lists across the whole cluster in one call.
	//
	// It is the default, and it is right for the group's own kinds because the
	// operator's cache is restricted to no namespace and its RBAC is a
	// ClusterRole: a StorageCluster in any namespace is this installation's,
	// wherever the operator itself runs.
	Everywhere Reach = iota

	// Occupied lists once per namespace the group's own objects were found in,
	// which is what [upgrade.Scope.Occupied] reports.
	//
	// It exists for the workload a StorageNodeSet owns. Those objects are
	// created in the set's namespace, so they follow the custom resources
	// rather than the operator, and they are kinds a cluster holds thousands
	// of. Listing every Secret and ConfigMap in a large cluster costs a great
	// deal and returns almost nothing this migration is about, so a kind
	// declared here waits for the pass that found the sets and reads only
	// where they are.
	Occupied
)

func (k Kind) ID() upgrade.ID {
	return k.RuleID
}
func (k Kind) Description() string {
	return k.Summary
}
func (k Kind) Requires() []upgrade.ID {
	return k.Needs
}

// describe says what is about to be read, in the form a report prints while it
// waits: the kind, and the namespace it is narrowed to.
func (k Kind) describe(s *upgrade.Scope) string {
	kind := strings.TrimSuffix(fmt.Sprintf("%T", k.List), "List")
	if idx := strings.LastIndex(kind, "."); idx >= 0 {
		kind = kind[idx+1:]
	}
	if k.Where == Occupied {
		return fmt.Sprintf("%s in %d namespace(s)", kind, len(s.Occupied()))
	}
	return kind + " across the cluster"
}

// Discover lists the kind and adopts what it found into the graph.
//
// A kind the API server does not serve is reported and skipped rather than
// failing the run. The alternative is that a cluster missing one CRD gets a
// preflight that says nothing about the other seventeen, and whether the CRDs
// are installed and established is a check of its own (§9.2) rather than an
// accident of which discoverer ran first.
func (k Kind) Discover(ctx context.Context, s *upgrade.Scope) error {
	// The kind is named before the read rather than after, because a list
	// against a large cluster is exactly the wait this reports through.
	s.Report.Item(k.describe(s))

	var base []client.ListOption
	if len(k.Labels) > 0 {
		base = append(base, client.MatchingLabels(k.Labels))
	}

	for _, namespace := range k.namespaces(s) {
		opts := base
		if namespace != "" {
			opts = append(opts, client.InNamespace(namespace))
		}

		// A fresh copy per call: the prototype is declared once in the catalog
		// and would otherwise carry one namespace's objects into the next.
		list, ok := k.List.DeepCopyObject().(client.ObjectList)
		if !ok {
			return fmt.Errorf("%s: %T is not a list", k.RuleID, k.List)
		}

		if err := s.Client.List(ctx, list, opts...); err != nil {
			if meta.IsNoMatchError(err) {
				s.Report.Progress("%s: the API server serves no such kind, and nothing was read", k.RuleID)
				return nil
			}
			return fmt.Errorf("listing for %s: %w", k.RuleID, err)
		}

		items, err := meta.ExtractList(list)
		if err != nil {
			return fmt.Errorf("reading the list for %s: %w", k.RuleID, err)
		}

		objects := make([]client.Object, 0, len(items))
		for _, item := range items {
			obj, ok := item.(client.Object)
			if !ok {
				return fmt.Errorf("%s returned a %T, which is not a Kubernetes object", k.RuleID, item)
			}
			objects = append(objects, obj)
		}
		s.Adopt(objects...)
	}
	return nil
}

// namespaces is where to read, and the empty string means the whole cluster in
// one call.
//
// An Occupied discoverer on a cluster where the group's own objects were not
// found reads nothing. That is correct rather than a gap: the workload it would
// look for belongs to custom resources that are not there.
func (k Kind) namespaces(s *upgrade.Scope) []string {
	if k.Where == Occupied {
		return s.Occupied()
	}
	return []string{""}
}
