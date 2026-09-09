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

	// Namespaced says the kind itself is namespaced. Every kind in the
	// storage.simplyblock.io group is today.
	Namespaced bool

	// View decides which of the scope's two graphs the objects go into, and
	// whether a namespaced kind is narrowed to the installation. It defaults to
	// [ViewInstallation], which is what all but two kinds want.
	View View

	// Labels narrows a cluster-wide list to what this installation owns. It is
	// how the worker Nodes are found without reading every Node in the cluster.
	Labels map[string]string

	// Needs names the discoverers whose objects this one reads.
	Needs []upgrade.ID
}

// View is which of the scope's graphs a discoverer fills.
type View int

const (
	// ViewInstallation reads the installation: a namespaced kind is narrowed to
	// the scope's namespace, and the objects go into the graph every check
	// reads. It is the default because a cluster may hold several independent
	// installations, and almost every question is about this one (§17).
	ViewInstallation View = iota

	// ViewClusterWide reads a namespaced kind across every namespace, into
	// [upgrade.Scope.ClusterWide]. Only the kinds whose derived identifiers
	// escape a namespace need it, and reading one costs a list against every
	// namespace in the cluster, so a kind is registered for it because a named
	// check cannot answer its question otherwise.
	ViewClusterWide
)

func (k Kind) ID() upgrade.ID         { return k.RuleID }
func (k Kind) Description() string    { return k.Summary }
func (k Kind) Requires() []upgrade.ID { return k.Needs }

// describe says what is about to be read, in the form a report prints while it
// waits: the kind, and the namespace it is narrowed to.
func (k Kind) describe(s *upgrade.Scope) string {
	kind := strings.TrimSuffix(fmt.Sprintf("%T", k.List), "List")
	if idx := strings.LastIndex(kind, "."); idx >= 0 {
		kind = kind[idx+1:]
	}
	if k.Namespaced && k.View == ViewInstallation {
		return kind + " in " + s.Namespace
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
	list, ok := k.List.DeepCopyObject().(client.ObjectList)
	if !ok {
		return fmt.Errorf("%s: %T is not a list", k.RuleID, k.List)
	}

	if k.View == ViewClusterWide && !k.Namespaced {
		// A cluster-scoped kind has no namespaces to be read across, so the
		// declaration is a mistake rather than a wider read.
		return fmt.Errorf("%s asks for a cluster-wide view of a kind that is not namespaced", k.RuleID)
	}

	var opts []client.ListOption
	if k.Namespaced && k.View == ViewInstallation {
		opts = append(opts, client.InNamespace(s.Namespace))
	}
	if len(k.Labels) > 0 {
		opts = append(opts, client.MatchingLabels(k.Labels))
	}

	// The kind is named before the read rather than after, because a list
	// against a large cluster is exactly the wait this reports through.
	s.Report.Item(k.describe(s))

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
	if k.View == ViewClusterWide {
		s.AdoptClusterWide(objects...)
	} else {
		s.Adopt(objects...)
	}
	return nil
}
