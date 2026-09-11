// §20's ownership migration, as three steps the runner orders through their
// requirements.
//
//	reparent-storage-nodes     the StorageNodes, which are the spine's one real
//	                           ownership edge
//	reparent-node-set-workload the DaemonSet, Services, EndpointSlices,
//	                           ServiceAccount, ConfigMaps, Secrets, and
//	                           Certificates the set owns
//	retire-storage-node-sets   the set itself, once both are done
//
// The requirement edges are load bearing rather than cosmetic. Kubernetes
// garbage collection removes a dependent when its owners are gone, and §16.1
// lists what depends on a StorageNodeSet, so deleting a set before its
// dependents have moved deletes the storage plane. The design says as much:
// the migration MUST NOT delete an old owner before every dependent intended to
// survive has been transferred and verified.
//
// Within one object the move is a single update rather than the design's
// two-stage add-then-remove. An update is atomic, so the object is never
// persisted without an owner and the garbage collector never observes a window
// to act in. What the two-stage ordering protects is the sequence between
// objects, and that is what the requirements express.

package steps

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/spine"
)

// The identities of the ownership steps.
const (
	IDReparentNodes    upgrade.ID = "reparent-storage-nodes"
	IDReparentWorkload upgrade.ID = "reparent-node-set-workload"
	IDRetireNodeSets   upgrade.ID = "retire-storage-node-sets"
)

// Ownership returns §20's steps, in the order their requirements impose.
func Ownership() []upgrade.Step {
	memo := &spineMemo{}
	return []upgrade.Step{
		reparentStep{
			id:      IDReparentNodes,
			summary: "moves each StorageNode from its StorageNodeSet to the StorageCluster",
			memo:    memo,
			subject: nodeSubject,
		},
		reparentStep{
			id:      IDReparentWorkload,
			summary: "moves the workload a StorageNodeSet owns to the StorageCluster",
			memo:    memo,
			subject: workloadSubject,
			needs:   []upgrade.ID{IDReparentNodes},
		},
		retireStep{
			memo:  memo,
			needs: []upgrade.ID{IDReparentNodes, IDReparentWorkload},
		},
	}
}

// target is what one subject's move looks like: the set it is leaving and the
// cluster it is joining. A nil target means the step has nothing to do with the
// subject.
type target struct {
	from upgrade.ObjectRef
	to   upgrade.ObjectRef
}

// nodeSubject resolves a StorageNode's move.
//
// A node already on its cluster resolves to a move that is already made rather
// than to nothing. Describe declines it either way, and the difference is what
// Done reports: a finished node and a node nothing is responsible for are very
// different answers to whether the migration is complete.
func nodeSubject(graph *spine.Spine, subject upgrade.Subject) *target {
	for _, node := range graph.Nodes {
		if node.Ref.Identity() != subject.Ref.Identity() {
			continue
		}
		if node.Controller != nil && node.Controller.Cluster != nil {
			return &target{from: node.Controller.Ref, to: node.Controller.Cluster.Ref}
		}
		if on := alreadyOn(graph, subject.Object); on != nil {
			return &target{from: *on, to: *on}
		}
		// A node no set owns, and on no cluster either. ownership-spine refuses
		// that before the migration runs, and there is no target to name here.
		return nil
	}
	return nil
}

// workloadSubject resolves the move of an object a set owns that is not a
// StorageNode.
func workloadSubject(graph *spine.Spine, subject upgrade.Subject) *target {
	if subject.IsUpgrade() || subject.Ref.GVK.Kind == nodeKind {
		// The nodes are the other step's, and they are the one dependent the
		// spine models as something other than a dependent.
		return nil
	}

	for _, set := range graph.Sets {
		if set.Cluster == nil {
			continue
		}
		for _, dependent := range set.Dependents {
			if dependent.Ref.Identity() != subject.Ref.Identity() {
				continue
			}
			if dependent.Rule == nil || dependent.Rule.Does != spine.Reparent {
				// Nothing says what becomes of it, which reparenting-is-safe
				// refuses, or it is a kind the target model deletes rather
				// than moves.
				return nil
			}
			return &target{from: set.Ref, to: set.Cluster.Ref}
		}
	}

	// Already moved. The spine models it as a dependent of the set it came
	// from, and it is not one any more, so it is found by where it landed.
	if !reparentable(subject.Ref.GVK.Kind) {
		return nil
	}
	if on := alreadyOn(graph, subject.Object); on != nil {
		return &target{from: *on, to: *on}
	}
	return nil
}

// alreadyOn returns the StorageCluster this object is controlled by, when the
// spine holds one by that name.
func alreadyOn(graph *spine.Spine, obj client.Object) *upgrade.ObjectRef {
	if obj == nil {
		return nil
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller == nil || !*ref.Controller || ref.Kind != clusterKind {
			continue
		}
		for _, cluster := range graph.Clusters {
			if cluster.Ref.Name == ref.Name && cluster.Ref.Namespace == obj.GetNamespace() {
				return &cluster.Ref
			}
		}
	}
	return nil
}

// reparentable reports whether a kind is one a StorageNodeSet owns and §16.1
// moves onto the cluster.
func reparentable(kind string) bool {
	for _, rule := range spine.Rules() {
		if rule.Kind.Kind == kind && rule.Does == spine.Reparent && kind != nodeKind {
			return true
		}
	}
	return false
}

// reparentStep moves one class of subject from its StorageNodeSet to the
// StorageCluster.
type reparentStep struct {
	id      upgrade.ID
	summary string
	memo    *spineMemo
	subject func(*spine.Spine, upgrade.Subject) *target
	needs   []upgrade.ID
}

func (r reparentStep) ID() upgrade.ID {
	return r.id
}
func (r reparentStep) Description() string {
	return r.summary
}
func (r reparentStep) Stage() upgrade.Stage {
	return upgrade.StageMigrate
}
func (r reparentStep) Phase() upgrade.Phase {
	return upgrade.PhaseOwnership
}
func (r reparentStep) Requires() []upgrade.ID {
	return r.needs
}

// Describe reports the move, and nothing for a subject already owned by its
// cluster. That is not a change to skip: it is a change that does not exist, so
// a rerun's plan holds only what is left.
func (r reparentStep) Describe(_ context.Context, s *upgrade.Scope, subject upgrade.Subject) (*upgrade.Action, error) {
	move := r.subject(r.memo.of(s), subject)
	if move == nil || controlledBy(subject.Object, move.to) {
		return nil, nil
	}
	return &upgrade.Action{
		Rule:   r.id,
		Verb:   upgrade.VerbReparent,
		Object: subject.Ref,
		Detail: fmt.Sprintf("owner: %s/%s → %s/%s",
			move.from.GVK.Kind, move.from.Name, move.to.GVK.Kind, move.to.Name),
	}, nil
}

// Done claims a subject this step is about and has already moved, which is what
// tells a finished subject from one nothing is responsible for.
func (r reparentStep) Done(_ context.Context, s *upgrade.Scope, subject upgrade.Subject) (bool, error) {
	move := r.subject(r.memo.of(s), subject)
	return move != nil && controlledBy(subject.Object, move.to), nil
}

// Validate refuses a move whose destination is not there to receive it. The
// checks of §18 ask the same of the graph as a whole, and this asks it of the
// object about to be written, because the two runs are not the same instant.
func (r reparentStep) Validate(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	move := r.subject(r.memo.of(s), subject)
	if move == nil {
		return fmt.Errorf("no move was resolved, and one was described")
	}
	if _, held := s.Graph.Get(move.to.Identity()); !held {
		return fmt.Errorf("its %s does not exist", move.to)
	}
	if move.to.UID == "" {
		return fmt.Errorf("%s has no UID, and an owner reference without one is not resolvable", move.to)
	}
	return nil
}

// Apply performs the move as one update.
//
// The graph is refreshed with what was written, so the steps that follow read
// the state this one produced rather than the state discovery found. The
// retirement is the caller that needs it: it asks whether a set still has
// dependents, and a stale graph would answer that it does.
func (r reparentStep) Apply(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	move := r.subject(r.memo.of(s), subject)
	if move == nil {
		return fmt.Errorf("no move was resolved, and one was described")
	}

	obj := subject.Object.DeepCopyObject().(client.Object)
	obj.SetOwnerReferences(reparented(obj.GetOwnerReferences(), move.from, move.to))
	if err := s.Client.Update(ctx, obj); err != nil {
		return fmt.Errorf("writing the owner reference: %w", err)
	}

	// The graph is refreshed and the spine is not. The spine describes the
	// state this phase started from, which is what every step's target was
	// resolved against, and rebuilding it mid-phase would leave Verify unable
	// to name the move it had just made.
	s.Adopt(obj)
	return nil
}

// Verify re-reads the object from the API rather than trusting what was
// written, because what a step has to confirm is what the cluster holds.
func (r reparentStep) Verify(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	move := r.subject(r.memo.of(s), subject)
	if move == nil {
		return fmt.Errorf("no move was resolved, and one was applied")
	}

	fresh := subject.Object.DeepCopyObject().(client.Object)
	if err := s.Client.Get(ctx, subject.Ref.Key(), fresh); err != nil {
		return fmt.Errorf("re-reading it: %w", err)
	}
	if !controlledBy(fresh, move.to) {
		return fmt.Errorf("it is not controlled by %s", move.to)
	}
	return nil
}

// retireStep deletes a StorageNodeSet once nothing that has to survive depends
// on it.
type retireStep struct {
	memo  *spineMemo
	needs []upgrade.ID
}

func (retireStep) ID() upgrade.ID {
	return IDRetireNodeSets
}
func (retireStep) Stage() upgrade.Stage {
	return upgrade.StageMigrate
}
func (retireStep) Phase() upgrade.Phase {
	return upgrade.PhaseOwnership
}

func (retireStep) Description() string {
	return "deletes a StorageNodeSet once everything it held has been reparented"
}

func (r retireStep) Requires() []upgrade.ID {
	return r.needs
}

// Describe reports the deletion for a set the graph still holds.
func (r retireStep) Describe(_ context.Context, s *upgrade.Scope, subject upgrade.Subject) (*upgrade.Action, error) {
	set := r.setFor(s, subject)
	if set == nil {
		return nil, nil
	}
	return &upgrade.Action{
		Rule:   IDRetireNodeSets,
		Verb:   upgrade.VerbDelete,
		Object: subject.Ref,
		Detail: "retired, its contents having moved to " + set.Cluster.Ref.String(),
	}, nil
}

// Done reports a set the cluster no longer holds. It is a live read because a
// deletion is the one change the graph cannot show: an object removed from the
// cluster stays in a graph that was built before it went.
func (r retireStep) Done(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) (bool, error) {
	if subject.Object == nil || subject.Ref.GVK.Kind != nodeSetKind {
		return false, nil
	}

	fresh := subject.Object.DeepCopyObject().(client.Object)
	err := s.Client.Get(ctx, subject.Ref.Key(), fresh)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("re-reading it: %w", err)
	}
	return false, nil
}

// Validate refuses while anything that has to survive still names this set as
// its only owner.
//
// This is the check §20's ordering exists for. Garbage collection removes a
// dependent when its owners are gone, so a set deleted with the DaemonSet still
// under it takes the storage plane with it.
func (r retireStep) Validate(_ context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	held := s.Graph.Dependents(subject.Ref.Identity())
	if len(held) == 0 {
		return nil
	}

	names := make([]string, 0, len(held))
	for _, id := range held {
		names = append(names, fmt.Sprintf("%s %s/%s", id.Kind, id.Namespace, id.Name))
	}
	return fmt.Errorf("%d object(s) still name it as an owner and would be garbage-collected with it: %v",
		len(held), names)
}

// Apply deletes the set.
func (r retireStep) Apply(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	if err := s.Client.Delete(ctx, subject.Object); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting it: %w", err)
	}
	return nil
}

// Verify confirms the set is gone and that what used to depend on it is not.
//
// The second half is §20's sixth step, and it is the one that would catch a
// reparenting this migration got wrong: the set deletes cleanly either way, and
// the difference shows in whether the DaemonSet is still there afterward.
func (r retireStep) Verify(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	fresh := subject.Object.DeepCopyObject().(client.Object)
	if err := s.Client.Get(ctx, subject.Ref.Key(), fresh); err == nil {
		return fmt.Errorf("it still exists")
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("re-reading it: %w", err)
	}

	for _, survivor := range r.survivors(s, subject) {
		obj := survivor.Object.DeepCopyObject().(client.Object)
		if err := s.Client.Get(ctx, survivor.Ref.Key(), obj); err != nil {
			return fmt.Errorf("%s was reparented off it and is gone: %w", survivor.Ref, err)
		}
	}
	return nil
}

// survivors are the objects this set used to hold, which the graph still
// records because Apply refreshed them as it moved them.
func (r retireStep) survivors(s *upgrade.Scope, subject upgrade.Subject) []upgrade.Subject {
	var out []upgrade.Subject
	for _, other := range s.Subjects() {
		if other.IsUpgrade() || other.Ref.Identity() == subject.Ref.Identity() {
			continue
		}
		if formerlyHeldBy(other.Object, subject.Ref) {
			out = append(out, other)
		}
	}
	return out
}

// setFor returns the spine's entry for a subject that is a StorageNodeSet with
// a cluster to have moved its contents to.
func (r retireStep) setFor(s *upgrade.Scope, subject upgrade.Subject) *spine.NodeSet {
	if subject.IsUpgrade() {
		return nil
	}
	for _, set := range r.memo.of(s).Sets {
		if set.Ref.Identity() == subject.Ref.Identity() && set.Cluster != nil {
			return set
		}
	}
	return nil
}

const (
	clusterKind = "StorageCluster"
	nodeSetKind = "StorageNodeSet"
	nodeKind    = "StorageNode"
)

// controlledBy reports whether this object's controller reference names the
// owner.
func controlledBy(obj client.Object, owner upgrade.ObjectRef) bool {
	if obj == nil {
		return false
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.Kind == owner.GVK.Kind && ref.Name == owner.Name {
			return true
		}
	}
	return false
}

// formerlyHeldBy reports whether this object names the owner at all, controller
// or not, which is what garbage collection acts on.
func formerlyHeldBy(obj client.Object, owner upgrade.ObjectRef) bool {
	if obj == nil {
		return false
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == owner.GVK.Kind && ref.Name == owner.Name {
			return true
		}
	}
	return false
}

// reparented is the new owner-reference list: the old owner dropped, and the
// new one added as the controller.
//
// One list rather than two writes. An update is atomic, so the object is never
// persisted without an owner and the garbage collector is never given a window
// to act in, which is what the design's add-then-remove ordering protects
// between objects rather than within one.
func reparented(refs []metav1.OwnerReference, from, to upgrade.ObjectRef) []metav1.OwnerReference {
	controller := true
	out := make([]metav1.OwnerReference, 0, len(refs)+1)
	for _, ref := range refs {
		if ref.Kind == from.GVK.Kind && ref.Name == from.Name {
			continue
		}
		// Only one reference may be the controller, and the new owner is about
		// to be it.
		ref.Controller = nil
		out = append(out, ref)
	}
	return append(out, metav1.OwnerReference{
		APIVersion: to.GVK.GroupVersion().String(),
		Kind:       to.GVK.Kind,
		Name:       to.Name,
		UID:        to.UID,
		Controller: &controller,
	})
}
