// What a rule is handed when it runs. Every check, step, and derivation reads
// the cluster through this one value, so that a test can substitute a fake
// client for the whole framework and a dry run can substitute a client that
// fails the test on any write.

package upgrade

import (
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Scope is the cluster a run operates on.
//
// It is passed by pointer and shared by every rule in a run, which is what lets
// a discoverer fill the graph that a later check reads. Nothing but discovery
// writes to it.
type Scope struct {
	// Client reads and writes the cluster. In a preflight it is a client that
	// refuses writes (see [ReadOnlyClient]), so a check that writes fails
	// rather than mutating a cluster the user was told nothing would change on.
	Client client.Client

	// Namespace is the installation being upgraded. Discovery is namespace-wide
	// rather than cluster-wide because every kind in the group but the
	// cluster-scoped additions is namespaced, and a cluster may hold several
	// independent installations.
	Namespace string

	// Graph is what discovery found inside the installation. It is empty until
	// a [Discoverer] has run.
	Graph *Graph

	// ClusterWide is the same kinds read across every namespace, and it holds
	// only the kinds that need it.
	//
	// It exists because two of §19.8's uniqueness routes escape a namespace. A
	// StorageCluster's name reaches the workers as a node label carrying
	// nothing else, so two clusters of one name in two namespaces claim the
	// same machines, and a kind that becomes cluster-scoped loses the namespace
	// that was keeping its objects apart. Neither is visible from inside one
	// installation.
	//
	// It is a second graph rather than a wider first one because almost every
	// check wants the installation and would draw a wrong conclusion from
	// another tenant's objects. A rule reads this one only when the question it
	// asks genuinely has no namespace in it.
	ClusterWide *Graph

	// Stage is the command being run, so a rule registered for more than one
	// can tell which it is in.
	Stage Stage

	// Options are the run's switches.
	Options Options

	// Log is where a rule writes what it is doing. User-facing output goes
	// through the [Reporter] instead: the log is for diagnosis and the reporter
	// is for the person watching.
	Log logr.Logger

	// Report is where findings, plans, and progress go.
	Report Reporter
}

// Options are the run's switches, carried here rather than read from flags in
// each rule so a rule stays testable without a command line.
type Options struct {
	// DryRun reports what would happen and writes nothing. It is implied by the
	// preflight stage and available to the others.
	DryRun bool

	// Skip names rules that are not to run. It is the escape hatch for a check
	// whose finding a user has decided to accept, and every skipped rule is
	// named in the report so the decision is visible afterward.
	Skip []ID

	// AcknowledgeOffline admits a StorageNode that is offline, which validation
	// otherwise refuses (§18).
	AcknowledgeOffline bool
}

// Skipped reports whether this rule was named on the command line.
func (o Options) Skipped(id ID) bool {
	for _, skip := range o.Skip {
		if skip == id {
			return true
		}
	}
	return false
}

// NewScope builds a scope with an empty graph, which is the state every run
// starts in.
func NewScope(c client.Client, namespace string, stage Stage, opts Options, log logr.Logger, report Reporter) *Scope {
	if report == nil {
		report = DiscardReporter{}
	}
	return &Scope{
		Client:      c,
		Namespace:   namespace,
		Graph:       NewGraph(),
		ClusterWide: NewGraph(),
		Stage:       stage,
		Options:     opts,
		Log:         log,
		Report:      report,
	}
}

// GVK resolves an object's kind through the scope's scheme. It exists because
// a typed object read through a controller-runtime client comes back with its
// TypeMeta cleared, so [RefOf] on a freshly read object would produce a
// reference with no kind in it.
func (s *Scope) GVK(obj client.Object) (schema.GroupVersionKind, error) {
	gvks, _, err := s.Client.Scheme().ObjectKinds(obj)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("the scheme does not know %T: %w", obj, err)
	}
	if len(gvks) == 0 {
		return schema.GroupVersionKind{}, fmt.Errorf("the scheme knows %T under no kind", obj)
	}
	return gvks[0], nil
}

// Ref builds a reference to an object read through this scope, filling in the
// kind the client cleared.
func (s *Scope) Ref(obj client.Object) ObjectRef {
	ref := RefOf(obj)
	if ref.GVK.Empty() {
		if gvk, err := s.GVK(obj); err == nil {
			ref.GVK = gvk
		}
	}
	return ref
}

// Adopt records objects in the installation's graph with their kind filled in,
// which is what a discoverer calls rather than [Graph.Add].
func (s *Scope) Adopt(objs ...client.Object) {
	s.adopt(s.Graph, objs)
}

// AdoptClusterWide records objects in [Scope.ClusterWide] instead. Only the
// discoverers of the kinds whose identifiers escape a namespace call it.
func (s *Scope) AdoptClusterWide(objs ...client.Object) {
	s.adopt(s.ClusterWide, objs)
}

// adopt fills in the kind the client cleared and records the objects.
func (s *Scope) adopt(graph *Graph, objs []client.Object) {
	for _, obj := range objs {
		if obj.GetObjectKind().GroupVersionKind().Empty() {
			if gvk, err := s.GVK(obj); err == nil {
				obj.GetObjectKind().SetGroupVersionKind(gvk)
			}
		}
	}
	graph.Add(objs...)
}
