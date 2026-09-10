// What a step acts on.
//
// Most steps act on an object: a StorageNode to reparent, a claim whose
// annotation keys move, a StorageNodeSet to retire. The steps of §9.1 act on
// the upgrade itself, since deploying the conversion webhook, applying the
// CRDs, handing the Helm release over, and upgrading the operator change the
// installation rather than anything in it.
//
// Both are subjects. A second interface for the ones with no object would mean
// two contracts, two runner paths, and a plan assembled from two walks that
// happen to agree, so the upgrade is a subject like any other and one walk
// covers everything.

package upgrade

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// upgradeGVK is the kind an upgrade subject reports, in this API's own group so
// that nothing mistakes it for a core kind.
func upgradeGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: APIGroup, Kind: UpgradeKind}
}

// UpgradeKind is the kind an upgrade subject reports. It is not a registered
// Kubernetes kind and nothing serves it: it exists so a plan line about the
// upgrade reads like every other plan line.
const UpgradeKind = "Upgrade"

// Subject is what a step is asked about.
type Subject struct {
	// Ref names it, and is what a plan and a finding print.
	Ref ObjectRef

	// Object is the live object, and is nil when the subject is the upgrade
	// itself. A step that reads it without checking is a step that would panic
	// on the one subject every run has.
	//
	// A step that enumerates its own subjects decides what it carries, and for
	// one that creates objects it is the object as it is to be written rather
	// than one the cluster already holds. There is no live object to point at
	// in that case, which is the whole reason the step enumerates.
	Object client.Object
}

// IsUpgrade reports whether this is the upgrade rather than an object in the
// cluster.
func (s Subject) IsUpgrade() bool { return s.Object == nil }

// String renders the subject the way a plan names it.
func (s Subject) String() string { return s.Ref.String() }

// TheUpgrade is the subject the steps of §9.1 act on. There is exactly one per
// run, and it is named for the namespace the operator's own furniture lives in,
// since that is what those steps change.
func TheUpgrade(namespace string) Subject {
	return Subject{Ref: ObjectRef{
		GVK:       upgradeGVK(),
		Namespace: namespace,
		Name:      "upgrade",
	}}
}

// SubjectOf wraps a discovered object.
func (s *Scope) SubjectOf(obj client.Object) Subject {
	return Subject{Ref: s.Ref(obj), Object: obj}
}

// Subjects is everything a step is asked about, in a stable order: the upgrade
// first, then the graph's objects, kinds sorted and objects in discovery order
// within a kind.
//
// The upgrade comes first because the steps that act on it come first: nothing
// is migrated before the cluster can run the operator that migrates it.
func (s *Scope) Subjects() []Subject {
	objects := s.Graph.Objects()

	out := make([]Subject, 0, len(objects)+1)
	out = append(out, TheUpgrade(s.Namespace))
	for _, obj := range objects {
		out = append(out, s.SubjectOf(obj))
	}
	return out
}

// Enumerator is a step whose subjects are not the run's.
//
// The graph holds what the cluster has, so a step that creates something acts
// on a subject no discovery can find: §11 applies the CRDs, and the one it
// creates for a kind that is new in v1alpha2 is exactly the one that is not
// installed yet. Without this, such a step would have to act on the upgrade and
// hide its real work in a sentence, which is the failure the per-object
// contract exists to prevent.
//
// It is optional. A step that does not implement it is asked about the run's
// subjects, which is what every step that acts on the cluster wants.
type Enumerator interface {
	// Subjects is what this step is asked about, in a stable order. A plan
	// printed twice from one cluster has to read the same both times.
	Subjects(ctx context.Context, s *Scope) ([]Subject, error)
}

// SubjectsFor is what a step is asked about: its own where it enumerates them,
// and the run's otherwise.
func SubjectsFor(ctx context.Context, s *Scope, step Step) ([]Subject, error) {
	enumerator, own := step.(Enumerator)
	if !own {
		return s.Subjects(), nil
	}

	subjects, err := enumerator.Subjects(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("step %q could not enumerate its subjects: %w", step.ID(), err)
	}
	return subjects, nil
}
