// Conversion of StorageClusterOps between this version and the v1alpha2 hub.
//
// Three properties are renames (design-property-renames.md §2.1, §2.2, and
// §2.5): the rolling-restart spec and status blocks lose their Node prefix, and
// the action enum is recased. See controlplane_conversion.go for why an
// unmapped enum value is passed through rather than rejected.
//
// The rest is the step machine of design-storagecluster.md §5.3 and §7, which
// this version cannot express at all, and it divides in two:
//
//   - The walk's shape. The hub states one immutable list and an index into it;
//     this version states two lists it drains from one into the other. The two
//     carry the same information and convert by arithmetic rather than by a
//     stash: processedNodes is nodes[:nodeIndex] and pendingNodes is the rest.
//   - Everything else. status.step, spec.abort, spec.cancelTask,
//     status.observedGeneration, and the Aborted phase have nowhere to go here,
//     so they are stashed on the way down and restored on the way up. The
//     Aborted phase is the sharp one: this version's Enum marker does not
//     accept the value, so writing it would make the object rejected at
//     admission rather than merely odd, and it is narrowed to Failed with the
//     true phase in its annotation.
//
// Two fields travel the other way. status.triggered and
// status.rollingRestart.phaseTriggered are this version's and the hub removed
// them (§6.2), so they stash on the way up under
// storage.simplyblock.io/v1alpha1-<field>. What is preserved is the text that
// was stored rather than any behavior: the hub's controller reads the persisted
// step instead, and every call it makes is skipped when its target is already
// at or past what that call would produce.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/simplyblock/atlas/statemachine"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The annotations holding the two status fields the hub removed.
const (
	annoV1Alpha1OpsTriggered      = "storage.simplyblock.io/v1alpha1-status.triggered"
	annoV1Alpha1OpsPhaseTriggered = "storage.simplyblock.io/v1alpha1-status.rollingRestart.phaseTriggered"
)

// The annotations holding the hub fields this version cannot express.
const (
	annoOpsStep      = "storage.simplyblock.io/conversion-status.step"
	annoOpsPhase     = "storage.simplyblock.io/conversion-status.phase"
	annoOpsObserved  = "storage.simplyblock.io/conversion-status.observedGeneration"
	annoOpsAbort     = "storage.simplyblock.io/conversion-spec.abort"
	annoOpsCancelTsk = "storage.simplyblock.io/conversion-spec.cancelTask"
)

// storageClusterOpsActionToHub maps this version's lowercase, hyphenated
// actions onto the hub's PascalCase ones. Every value is listed, because unlike
// a phase this enum is user-authored and appears in scripts and runbooks.
//
// CancelTask has no row: the action is the hub's addition and this version
// never accepted it, so it passes through and is rejected by this version's own
// Enum marker, which is the correct outcome for an operation this version
// cannot perform.
var storageClusterOpsActionToHub = map[string]string{
	"activate":             string(v1alpha2.StorageClusterOpsActionActivate),
	"expand":               string(v1alpha2.StorageClusterOpsActionExpand),
	"shutdown":             string(v1alpha2.StorageClusterOpsActionShutdown),
	"start":                string(v1alpha2.StorageClusterOpsActionStart),
	"restart":              string(v1alpha2.StorageClusterOpsActionRestart),
	"node-rolling-restart": string(v1alpha2.StorageClusterOpsActionRollingRestart),
}

// storageClusterOpsActionFromHub is the inverse, derived so the two cannot
// disagree about a value.
var storageClusterOpsActionFromHub = invertStringMap(storageClusterOpsActionToHub)

// nodePhaseToStep reads this version's per-node phase strings as the hub's
// steps. It covers only the values a real v1alpha1 object can hold, and it is
// consulted only when no step was stashed, which is to say for an operation
// that was in flight when the upgrade ran.
var nodePhaseToStep = map[string]string{
	"snode-refresh":      string(v1alpha2.StorageClusterOpsStepRefreshingPod),
	"snode-refresh-wait": string(v1alpha2.StorageClusterOpsStepAwaitingPod),
	"shutting-down":      string(v1alpha2.StorageClusterOpsStepShuttingDownNode),
	"restarting":         string(v1alpha2.StorageClusterOpsStepRestartingNode),
	"rebalancing":        string(v1alpha2.StorageClusterOpsStepRebalancing),
}

// stepToNodePhase projects a hub step back onto a per-node phase, so that a
// v1alpha1 reader sees where the walk is rather than an empty field. It is not
// the inverse of nodePhaseToStep and cannot be: CheckingPeers has no v1alpha1
// spelling of its own, because the peer check used to happen inside the
// shutting-down phase. The projection is display only, since the stash carries
// the real step.
var stepToNodePhase = map[string]string{
	string(v1alpha2.StorageClusterOpsStepCheckingPeers):    "shutting-down",
	string(v1alpha2.StorageClusterOpsStepShuttingDownNode): "shutting-down",
	string(v1alpha2.StorageClusterOpsStepRefreshingPod):    "snode-refresh",
	string(v1alpha2.StorageClusterOpsStepAwaitingPod):      "snode-refresh-wait",
	string(v1alpha2.StorageClusterOpsStepRestartingNode):   "restarting",
	string(v1alpha2.StorageClusterOpsStepRebalancing):      "rebalancing",
}

// ConvertTo converts this StorageClusterOps to the v1alpha2 hub.
func (src *StorageClusterOps) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha2.StorageClusterOps)

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	stashOpsFlag(&dst.ObjectMeta, annoV1Alpha1OpsTriggered, src.Status.Triggered)

	dst.Spec = v1alpha2.StorageClusterOpsSpec{
		ClusterRef: src.Spec.ClusterRef,
		Action: v1alpha2.StorageClusterOpsAction(
			mapOrPassThrough(storageClusterOpsActionToHub, src.Spec.Action),
		),
	}
	// An absent block stays absent: an operation of another action must not
	// gain a rolling-restart block it never had.
	if src.Spec.NodeRollingRestart != nil {
		dst.Spec.RollingRestart = &v1alpha2.RollingRestartSpec{
			RefreshSNodeAPI: src.Spec.NodeRollingRestart.RefreshSNodeAPI,
		}
	}

	dst.Status = v1alpha2.StorageClusterOpsStatus{
		Phase:       v1alpha2.StorageClusterOpsPhase(src.Status.Phase),
		Message:     src.Status.Message,
		StartedAt:   src.Status.StartedAt,
		CompletedAt: src.Status.CompletedAt,
	}
	if s := src.Status.NodeRollingRestartStatus; s != nil {
		stashOpsFlag(&dst.ObjectMeta, annoV1Alpha1OpsPhaseTriggered, s.PhaseTriggered)
		dst.Status.RollingRestart = &v1alpha2.RollingRestartStatus{
			// The walk keeps the order it was planned in: the nodes already
			// done, then the ones still to come.
			Nodes:     append(append([]string{}, s.ProcessedNodes...), s.PendingNodes...),
			NodeIndex: int32(len(s.ProcessedNodes)),
		}
		if step, ok := nodePhaseToStep[s.NodePhase]; ok {
			dst.Status.Step = statemachine.KubeSnapshot{State: step}
		}
	}

	return restoreOpsHubOnly(&dst.ObjectMeta, dst)
}

// ConvertFrom converts the v1alpha2 hub into this StorageClusterOps.
func (dst *StorageClusterOps) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha2.StorageClusterOps)

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()

	dst.Spec = StorageClusterOpsSpec{
		ClusterRef: src.Spec.ClusterRef,
		Action:     mapOrPassThrough(storageClusterOpsActionFromHub, string(src.Spec.Action)),
	}
	if src.Spec.RollingRestart != nil {
		dst.Spec.NodeRollingRestart = &NodeRollingRestartSpec{
			RefreshSNodeAPI: src.Spec.RollingRestart.RefreshSNodeAPI,
		}
	}

	dst.Status = StorageClusterOpsStatus{
		Phase:       narrowOpsPhase(src.Status.Phase),
		Triggered:   unstashRemoved(&dst.ObjectMeta, annoV1Alpha1OpsTriggered) == stashedTrue,
		Message:     src.Status.Message,
		StartedAt:   src.Status.StartedAt,
		CompletedAt: src.Status.CompletedAt,
	}
	phaseTriggered := unstashRemoved(&dst.ObjectMeta, annoV1Alpha1OpsPhaseTriggered) == stashedTrue
	if s := src.Status.RollingRestart; s != nil {
		index := int(s.NodeIndex)
		if index > len(s.Nodes) {
			index = len(s.Nodes)
		}
		dst.Status.NodeRollingRestartStatus = &NodeRollingRestartStatus{
			ProcessedNodes: append([]string{}, s.Nodes[:index]...),
			PendingNodes:   append([]string{}, s.Nodes[index:]...),
			NodePhase:      stepToNodePhase[src.Status.Step.State],
			PhaseTriggered: phaseTriggered,
		}
	}

	return stashOpsHubOnly(&dst.ObjectMeta, src)
}

// stashOpsHubOnly writes every hub field this version has nowhere to put. A
// field at its zero value writes no annotation, so an operation that reached
// none of them is not given metadata it never had.
func stashOpsHubOnly(meta *metav1.ObjectMeta, src *v1alpha2.StorageClusterOps) error {
	if err := stashOpsStep(meta, src.Status.Step); err != nil {
		return err
	}
	for _, field := range []struct {
		key   string
		value any
	}{
		{annoOpsObserved, src.Status.ObservedGeneration},
		{annoOpsCancelTsk, src.Spec.CancelTask},
	} {
		if err := stash(meta, field.key, field.value); err != nil {
			return err
		}
	}

	// Only the phase this version cannot spell is recorded. Every other value
	// survives the narrowing, and an annotation for each would be noise on
	// every operation that ever ran.
	if src.Status.Phase == v1alpha2.StorageClusterOpsPhaseAborted {
		stashRemoved(meta, annoOpsPhase, string(src.Status.Phase))
	} else {
		clear(meta, annoOpsPhase)
	}

	// spec.abort is a bool rather than a pointer, so false and unset are the
	// same value and only true is worth a note.
	if src.Spec.Abort {
		stashRemoved(meta, annoOpsAbort, stashedTrue)
	} else {
		clear(meta, annoOpsAbort)
	}
	return nil
}

// stashOpsStep records the step only when nodePhase cannot carry it.
//
// The projection is one-way, so most of the rolling restart's steps survive
// being written to nodePhase and read back, and annotating those would put a
// note on every operation that ever ran. What does not survive is a step with a
// deadline, a step of any action other than the rolling restart, and
// CheckingPeers, which shares a v1alpha1 spelling with ShuttingDownNode.
func stashOpsStep(meta *metav1.ObjectMeta, step statemachine.KubeSnapshot) error {
	if step.Deadline == nil && nodePhaseToStep[stepToNodePhase[step.State]] == step.State {
		clear(meta, annoOpsStep)
		return nil
	}
	return stash(meta, annoOpsStep, step)
}

// restoreOpsHubOnly reads them back and removes the annotations, so an object
// converted up carries the fields rather than both the fields and the notes
// about them.
func restoreOpsHubOnly(meta *metav1.ObjectMeta, dst *v1alpha2.StorageClusterOps) error {
	// The stash wins where there is one, because only it carries a deadline
	// and the steps of an action other than the rolling restart. ConvertTo has
	// already read status.nodeRollingRestartStatus.nodePhase for an object a
	// real v1alpha1 client wrote.
	var step statemachine.KubeSnapshot
	if err := unstash(meta, annoOpsStep, &step); err != nil {
		return err
	}
	if step.State != "" || step.Deadline != nil {
		dst.Status.Step = step
	}

	for _, field := range []struct {
		key    string
		target any
	}{
		{annoOpsObserved, &dst.Status.ObservedGeneration},
		{annoOpsCancelTsk, &dst.Spec.CancelTask},
	} {
		if err := unstash(meta, field.key, field.target); err != nil {
			return err
		}
	}

	if phase := unstashRemoved(meta, annoOpsPhase); phase != "" {
		dst.Status.Phase = v1alpha2.StorageClusterOpsPhase(phase)
	}
	dst.Spec.Abort = unstashRemoved(meta, annoOpsAbort) == stashedTrue
	return nil
}

// narrowOpsPhase maps a hub phase onto one this version's Enum accepts. Only
// Aborted needs it, and it reads as Failed here: the operation did stop before
// finishing, which is the closest true statement this version can make, and the
// annotation carries the distinction.
func narrowOpsPhase(phase v1alpha2.StorageClusterOpsPhase) StorageClusterOpsPhase {
	if phase == v1alpha2.StorageClusterOpsPhaseAborted {
		return StorageClusterOpsPhaseFailed
	}
	return StorageClusterOpsPhase(phase)
}

// stashOpsFlag records one of the two write-ahead flags this version keeps and
// the hub removed. Only true is written: the field is a plain bool here, so
// false and unset are the same value, and an annotation for it would appear on
// every operation that ever ran.
func stashOpsFlag(meta *metav1.ObjectMeta, key string, value bool) {
	if !value {
		clear(meta, key)
		return
	}
	stashRemoved(meta, key, stashedTrue)
}
