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

	// Namespace is where the operator runs, which is not where its custom
	// resources live.
	//
	// The manager restricts its cache to no namespace and its RBAC is a
	// ClusterRole, so one operator reconciles the whole cluster and a
	// StorageCluster in any namespace is this installation's. WATCH_NAMESPACE
	// is set by the chart and read nowhere in the Go, so it bounds nothing.
	//
	// What this namespace is still for is the operator's own furniture: the
	// Helm release, the conversion webhook §9 deploys, the ControlPlane the
	// chart installs, and the migration record of §22.1.
	Namespace string

	// Graph is what discovery found, across every namespace. It is empty until
	// a [Discoverer] has run.
	//
	// There is one graph and not one per namespace, because there is one
	// installation. A second operator in a second namespace would watch the
	// same cluster-wide set of objects as the first and fight it, so two
	// independent installations in one cluster is not a state this product
	// reaches.
	Graph *Graph

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
		Client:    c,
		Namespace: namespace,
		Graph:     NewGraph(),
		Stage:     stage,
		Options:   opts,
		Log:       log,
		Report:    report,
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

// Adopt records objects in the graph with their kind filled in, which is what a
// discoverer calls rather than [Graph.Add].
func (s *Scope) Adopt(objs ...client.Object) {
	for _, obj := range objs {
		if obj.GetObjectKind().GroupVersionKind().Empty() {
			if gvk, err := s.GVK(obj); err == nil {
				obj.GetObjectKind().SetGroupVersionKind(gvk)
			}
		}
	}
	s.Graph.Add(objs...)
}

// Occupied is the namespaces this installation's custom resources were found
// in, sorted.
//
// It is derived from the graph rather than configured, because where the custom
// resources live is a fact about the cluster and not a decision anybody made:
// an operator in one namespace reconciles a StorageCluster in another, and the
// workload a StorageNodeSet owns is created in the set's namespace rather than
// the operator's.
//
// It is what the second pass of discovery narrows to. Reading the ConfigMaps,
// Secrets, and Services of every namespace in a large cluster costs a great
// deal and returns almost nothing this migration is about.
func (s *Scope) Occupied() []string {
	return s.Graph.Namespaces(APIGroup)
}
