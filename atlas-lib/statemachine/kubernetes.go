// Kubernetes forms of [Snapshot]. A custom resource cannot hold the snapshot the
// machine produces, for two reasons that pull in opposite directions: its
// deadline has to be a [k8s.io/apimachinery/pkg/apis/meta/v1.Time] to serialize
// the way every other timestamp in a status does, and its state cannot be a type
// parameter, because controller-gen refuses to render a generic type into a
// schema. This file is the one place that conversion lives, so that a CRD embeds
// a shared type instead of each kind declaring a near-identical struct of its
// own.
//
// KubeSnapshot is deliberately not generic, and no marker makes a generic one
// work. controller-gen v0.21.0 fails with "unsupported AST kind *ast.IndexExpr"
// on a field typed Snapshot[Step], on a defined type over that instantiation,
// and on an alias to it alike, and an alias that does resolve then fails on the
// type parameter having no schema. A non-generic type renders, and ToKube and
// FromKube put the typing back at the boundary.
//
// A kind embeds it directly, and states its own step values as a CEL rule at the
// use site, which is what an Enum marker would do if a marker could reach a
// field of a shared type:
//
//	type StorageNodeOpsStatus struct {
//		// Step is the position of the running action's state machine.
//		// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Validating','Suspending','Promoting']",message="unknown step"
//		// +optional
//		Step statemachine.KubeSnapshot `json:"step,omitempty"`
//	}
//
// That rule is the one cost of not declaring the struct per kind, and it is paid
// in the schema rather than in Go: [Machine.Restore] still rejects a state the
// graph does not declare, and [FromKube] still yields a typed state.
//
// Doc comments here stay short on purpose. controller-gen copies a type's
// comment into every CRD that embeds it, so the reasoning lives in this header,
// which it does not read.

package statemachine

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KubeSnapshot is the durable position of a state machine as a custom resource
// stores it: a state, and the instant it expires. The two travel together by
// construction, because persisting one without the other yields a state that can
// never time out. See the file header for why it is not generic.
type KubeSnapshot struct {
	// State is the state the machine was in. Empty means the resource has not
	// been reconciled yet, and restores to the graph's initial state.
	// +optional
	State string `json:"state,omitempty"`

	// Deadline is when that state expires, absent when it has none. It is an
	// absolute instant, so a state whose deadline passed while the controller
	// was down restores as already expired.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
}

// DeepCopyInto writes a deep copy into out. It is hand-written because
// atlas-lib runs no deepcopy generator, and a consumer's generated
// DeepCopyInto calls this one for the field.
func (in *KubeSnapshot) DeepCopyInto(out *KubeSnapshot) {
	*out = *in
	if in.Deadline != nil {
		out.Deadline = in.Deadline.DeepCopy()
	}
}

// DeepCopy returns a deep copy of the receiver, or nil for a nil receiver.
func (in *KubeSnapshot) DeepCopy() *KubeSnapshot {
	if in == nil {
		return nil
	}
	out := new(KubeSnapshot)
	in.DeepCopyInto(out)
	return out
}

// ToKube converts a snapshot into the form a custom resource stores. A snapshot
// with no deadline becomes an absent one rather than a zero timestamp, so an
// unbounded state does not serialize as an instant in 1970.
//
// Take it from [Machine.Snapshot] after reconciling:
//
//	ops.Status.Step = statemachine.ToKube(sm.Snapshot())
func ToKube[S ~string](snap Snapshot[S]) KubeSnapshot {
	kube := KubeSnapshot{State: string(snap.State)}
	if !snap.Deadline.IsZero() {
		kube.Deadline = &metav1.Time{Time: snap.Deadline}
	}
	return kube
}

// FromKube converts a stored snapshot back into the machine's own form, typing
// the state as it goes. The type argument is what restores the compile-time
// guarantee the string in the resource gives up:
//
//	sm, err := graphs.FromSnapshot(ctx, action, statemachine.FromKube[Step](ops.Status.Step))
//
// An absent deadline becomes a zero one, which is a state that never expires,
// and that is the same reading [Machine.Restore] gives it. A state string the
// graph does not declare is not rejected here: it is rejected by Restore, which
// is where the graph is known.
func FromKube[S ~string](kube KubeSnapshot) Snapshot[S] {
	snap := Snapshot[S]{State: S(kube.State)}
	if kube.Deadline != nil {
		snap.Deadline = kube.Deadline.Time
	}
	return snap
}

// KubeDeadline is the deadline a stored snapshot carries, and whether it has
// one. It exists so a caller can report how long a state has left without
// converting the whole snapshot.
func (in KubeSnapshot) KubeDeadline() (time.Time, bool) {
	if in.Deadline == nil {
		return time.Time{}, false
	}
	return in.Deadline.Time, true
}

// DeclaredStates returns every state a graph declares, as sorted strings. It
// exists for the check a shared [KubeSnapshot] makes necessary: the step values
// live in the graph, in the kind's `Enum` marker, and in the CEL rule at the use
// site, and nothing but a test makes those three agree.
//
//	func TestStepsAgree(t *testing.T) {
//		want := statemachine.DeclaredStates(graphs)
//		// compare against the Enum marker's values and the CEL rule's list
//	}
func DeclaredStates[S ~string](config Config[S]) []string {
	return stateNames(config.States)
}

// DeclaredMultiStates returns the union of every action's declared states, as
// sorted strings. It is the set a kind's step enum has to cover, because one
// status field serves every action.
func DeclaredMultiStates[S ~string](graphs MultiConfig[S]) []string {
	seen := make(map[S]StateDef[S])
	for _, config := range graphs {
		for state, def := range config.States {
			seen[state] = def
		}
	}
	return stateNames(seen)
}
