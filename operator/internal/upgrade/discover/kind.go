// The one discoverer every kind uses. It is a value rather than a function per
// kind because what differs between listing StorageNodes and listing
// StoragePools is a list type and a name, and eighteen copies of the same loop
// is eighteen places for a kind to be listed in the wrong namespace.

package discover

import (
	"context"
	"fmt"

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

	// Namespaced lists within the installation's namespace. Every kind in the
	// storage.simplyblock.io group is namespaced today, and discovery is
	// namespace-wide rather than cluster-wide because a cluster may hold
	// several independent installations (§17).
	//
	// A cluster-scoped kind, and a core kind an installation only partly owns,
	// lists across the cluster and narrows with Labels instead.
	Namespaced bool

	// Labels narrows a cluster-wide list to what this installation owns. It is
	// how the worker Nodes are found without reading every Node in the cluster.
	Labels map[string]string

	// Needs names the discoverers whose objects this one reads.
	Needs []upgrade.ID
}

func (k Kind) ID() upgrade.ID         { return k.RuleID }
func (k Kind) Description() string    { return k.Summary }
func (k Kind) Requires() []upgrade.ID { return k.Needs }

// Discover lists the kind and adopts what it found into the graph.
//
// A kind the API server does not serve is reported and skipped rather than
// failing the run. The alternative is that a cluster missing one CRD gets a
// preflight that says nothing about the other seventeen, and whether the CRDs
// are installed and established is a check of its own (§9.2) rather than an
// accident of which discoverer ran first.
func (k Kind) Discover(ctx context.Context, s *upgrade.Scope) error {
	list, ok := k.List.DeepCopyObject().(client.ObjectList)
	if !ok {
		return fmt.Errorf("%s: %T is not a list", k.RuleID, k.List)
	}

	var opts []client.ListOption
	if k.Namespaced {
		opts = append(opts, client.InNamespace(s.Namespace))
	}
	if len(k.Labels) > 0 {
		opts = append(opts, client.MatchingLabels(k.Labels))
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
	return nil
}
