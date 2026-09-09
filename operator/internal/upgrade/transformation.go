// The object-mapping extension point. §16 lists the resource-model changes no
// CRD conversion can carry: a kind that was renamed, a kind that was absorbed
// into an action of another, and an annotation key that moved to a new prefix.
// Each of them is one object read and one object written, so each is one
// implementation of this interface rather than a step of its own.

package upgrade

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Disposition is what becomes of a source object once its target has been
// written and verified.
type Disposition string

const (
	// DispositionDelete removes the source. It is what a renamed or absorbed
	// kind does, and the delete happens only after the target is verified
	// (§21).
	DispositionDelete Disposition = "delete"

	// DispositionKeep leaves the source in place. It is what a rewritten
	// annotation key does: §16.3 writes the new key and leaves the old one for
	// the deprecation window, so the operator that still reads it keeps
	// working.
	DispositionKeep Disposition = "keep"
)

// Transformation maps one object into the object that replaces it.
//
// Transform MUST be a pure function of its input: the runner calls it during
// the plan as well as during the migration, and a transformation that reads the
// clock or the cluster would show a user one thing and do another. Where a
// transformation genuinely needs a lookup, such as resolving a pool name to a
// UUID, it resolves it during discovery and reads the result from the graph.
type Transformation interface {
	Rule

	// Source is the kind read.
	Source() schema.GroupVersionKind

	// Target is the kind written. It equals Source for a transformation that
	// rewrites an object in place.
	Target() schema.GroupVersionKind

	// Applies reports whether this source object is one this transformation
	// handles. A transformation over a kind that only some objects of need,
	// such as a volume handle carrying a pool name rather than a UUID, says so
	// here rather than returning a nil object from Transform.
	Applies(source client.Object) bool

	// Transform returns the object to write. The returned object carries its
	// own name and namespace, which is where a kind becoming cluster-scoped
	// decides what its objects are called.
	Transform(ctx context.Context, s *Scope, source client.Object) (client.Object, error)

	// Disposition is what becomes of the source once the target is verified.
	Disposition() Disposition
}
