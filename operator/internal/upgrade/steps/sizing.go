// Stamping each StorageNode with the sizing its cluster was built with.
//
// spec.config.sizing is required on the v1alpha2 StorageNode and has no v1alpha1
// spelling at all, because both values lived on the StorageCluster and the node
// held no copy (design-storagenode.md §3.1). A node stored as v1alpha1 therefore
// converts up with no sizing, and the first write of it afterward is refused by
// the field's own Required marker.
//
// The conversion cannot fill it in. It has no client to read the cluster with, and
// a conversion may not depend on one: it runs inside the API server's request path
// against whatever object was handed to it. So the value is carried the way every
// other hub field this version cannot express is carried — in the conversion's
// stash annotation, which ConvertTo reads back into the field — and writing that
// annotation is what this step does.
//
// It runs after reparent-storage-nodes, because the cluster a node's sizing comes
// from is the one that step establishes as its controller owner. A node that has
// not been reparented has no cluster to read, which is the same ordering the
// conversion's own fallback depends on.

package steps

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// IDStampNodeSizing is the step's identity.
const IDStampNodeSizing upgrade.ID = "stamp-storage-node-sizing"

// annoNodeSizing is the annotation api/v1alpha1/storagenode_conversion.go reads
// the sizing back out of. The two have to spell it the same way or the stamp is
// written somewhere nothing looks.
const annoNodeSizing = "storage.simplyblock.io/conversion-spec.config.sizing"

// Sizing returns the step.
func Sizing() []upgrade.Step { return []upgrade.Step{stampNodeSizing{}} }

// stampNodeSizing writes each node's sizing from its cluster.
type stampNodeSizing struct{}

func (stampNodeSizing) ID() upgrade.ID       { return IDStampNodeSizing }
func (stampNodeSizing) Stage() upgrade.Stage { return upgrade.StageMigrate }
func (stampNodeSizing) Phase() upgrade.Phase { return upgrade.PhaseTransforming }

// Requires names the reparent, because the cluster this reads is the owner that
// step establishes.
func (stampNodeSizing) Requires() []upgrade.ID { return []upgrade.ID{IDReparentNodes} }

func (stampNodeSizing) Description() string {
	return "stamps each StorageNode with the vCPU count and huge-page floor its StorageCluster was built with"
}

// Describe reports the stamp a node is missing, and nothing for one that carries
// it already. A rerun's plan therefore holds the outstanding work rather than what
// was originally intended.
func (s stampNodeSizing) Describe(
	ctx context.Context, scope *upgrade.Scope, subject upgrade.Subject,
) (*upgrade.Action, error) {
	if !s.applies(subject) || s.stamped(subject) {
		return nil, nil
	}
	sizing, err := s.sizingFor(ctx, scope, subject)
	if err != nil {
		// A cluster that cannot be read is Validate's to refuse. Describing
		// nothing here would drop the node from the plan silently, which is the
		// one outcome worse than a refusal.
		return &upgrade.Action{
			Rule:   IDStampNodeSizing,
			Verb:   upgrade.VerbAnnotate,
			Object: subject.Ref,
			Detail: "sizing unresolved: " + err.Error(),
		}, nil
	}
	return &upgrade.Action{
		Rule:   IDStampNodeSizing,
		Verb:   upgrade.VerbAnnotate,
		Object: subject.Ref,
		Detail: describeSizing(sizing),
	}, nil
}

// Done claims a node that already carries the stamp, read off the subject rather
// than off a record, so a run killed anywhere resumes correctly.
func (s stampNodeSizing) Done(
	_ context.Context, _ *upgrade.Scope, subject upgrade.Subject,
) (bool, error) {
	return s.applies(subject) && s.stamped(subject), nil
}

// Validate refuses a node whose cluster states no core count.
//
// The field is Required on the hub with a minimum of four, so a stamp built from
// a cluster that states nothing would write a sizing the next admission refuses —
// which is the same failure this step exists to prevent, moved one step later and
// made harder to attribute.
func (s stampNodeSizing) Validate(
	ctx context.Context, scope *upgrade.Scope, subject upgrade.Subject,
) error {
	if !s.applies(subject) || s.stamped(subject) {
		return nil
	}
	_, err := s.sizingFor(ctx, scope, subject)
	return err
}

// Apply writes the annotation, leaving everything else on the object alone.
func (s stampNodeSizing) Apply(
	ctx context.Context, scope *upgrade.Scope, subject upgrade.Subject,
) error {
	sizing, err := s.sizingFor(ctx, scope, subject)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(sizing)
	if err != nil {
		return fmt.Errorf("encoding the sizing: %w", err)
	}

	object, ok := subject.Object.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not a Kubernetes object", subject.Object)
	}
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[annoNodeSizing] = string(encoded)
	object.SetAnnotations(annotations)

	if err := scope.Client.Update(ctx, object); err != nil {
		return fmt.Errorf("writing the sizing stamp: %w", err)
	}
	scope.Adopt(object)
	return nil
}

// Verify re-reads the node and checks the stamp decodes to a usable sizing.
//
// Decoding it rather than asserting the key is present is the point: an
// annotation that is there and does not parse is read by the conversion as no
// sizing at all, which is the state this step exists to leave behind.
func (s stampNodeSizing) Verify(
	ctx context.Context, scope *upgrade.Scope, subject upgrade.Subject,
) error {
	fresh, ok := subject.Object.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not a Kubernetes object", subject.Object)
	}
	if err := scope.Client.Get(ctx, subject.Ref.Key(), fresh); err != nil {
		return fmt.Errorf("re-reading it: %w", err)
	}

	raw, carried := fresh.GetAnnotations()[annoNodeSizing]
	if !carried {
		return fmt.Errorf("%s was not written", annoNodeSizing)
	}
	var sizing simplyblockv1alpha2.StorageNodeSizing
	if err := json.Unmarshal([]byte(raw), &sizing); err != nil {
		return fmt.Errorf("%s does not decode as a sizing: %w", annoNodeSizing, err)
	}
	if sizing.VCPUCount == nil {
		return fmt.Errorf("%s carries no vcpuCount", annoNodeSizing)
	}
	return nil
}

// applies reports whether this subject is a StorageNode.
func (stampNodeSizing) applies(subject upgrade.Subject) bool {
	return !subject.IsUpgrade() && subject.Object != nil &&
		subject.Ref.GVK.Kind == nodeKind
}

// stamped reports whether the node already carries a decodable sizing.
//
// A malformed annotation counts as absent, so a stamp somebody hand-edited into
// nonsense is rewritten rather than trusted.
func (stampNodeSizing) stamped(subject upgrade.Subject) bool {
	raw, carried := subject.Object.GetAnnotations()[annoNodeSizing]
	if !carried {
		return false
	}
	var sizing simplyblockv1alpha2.StorageNodeSizing
	if err := json.Unmarshal([]byte(raw), &sizing); err != nil {
		return false
	}
	return sizing.VCPUCount != nil
}

// sizingFor reads the cluster the node belongs to and returns the sizing it was
// built with.
func (stampNodeSizing) sizingFor(
	ctx context.Context, scope *upgrade.Scope, subject upgrade.Subject,
) (simplyblockv1alpha2.StorageNodeSizing, error) {
	owner := controllingCluster(subject.Object)
	if owner == "" {
		return simplyblockv1alpha2.StorageNodeSizing{}, fmt.Errorf(
			"no StorageCluster owns this node, so there is no sizing to stamp from; "+
				"%s has not run", IDReparentNodes)
	}

	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: subject.Ref.Namespace, Name: owner}
	if err := scope.Client.Get(ctx, key, &cluster); err != nil {
		return simplyblockv1alpha2.StorageNodeSizing{},
			fmt.Errorf("reading StorageCluster %s: %w", owner, err)
	}
	if cluster.Spec.VCPUCount == nil {
		return simplyblockv1alpha2.StorageNodeSizing{}, fmt.Errorf(
			"StorageCluster %s states no spec.vcpuCount, so a node stamped from it "+
				"would be refused by the field's own minimum", owner)
	}

	count := *cluster.Spec.VCPUCount
	return simplyblockv1alpha2.StorageNodeSizing{
		VCPUCount:        &count,
		MinHugePagesSize: cluster.Spec.MinHugePagesSize,
	}, nil
}

// controllingCluster names the StorageCluster that owns this object, or the empty
// string when none does.
func controllingCluster(object client.Object) string {
	for _, ref := range object.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.Kind == clusterKind {
			return ref.Name
		}
	}
	return ""
}

// describeSizing renders the stamp for a plan, so a reader sees the numbers the
// step would write rather than that it would write something.
func describeSizing(sizing simplyblockv1alpha2.StorageNodeSizing) string {
	detail := fmt.Sprintf("vcpuCount=%d", *sizing.VCPUCount)
	if sizing.MinHugePagesSize != "" {
		detail += ", minHugePagesSize=" + sizing.MinHugePagesSize
	}
	return detail
}
