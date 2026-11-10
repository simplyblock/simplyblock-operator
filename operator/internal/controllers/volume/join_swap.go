// The JoinConsistencyGroup swap: every host consuming a moving volume is given
// the target's paths, holds the volume's I/O, the control plane moves the
// namespaces, and each host is pointed at the subsystem they ended up in and
// released.
//
// The window between the first hold and the last release is bounded by one
// absolute deadline, persisted before any host is asked to hold, and given to
// every host and to the control plane. A host past it resumes by itself, and
// the control plane refuses to start a move past it, so neither a lost
// operator nor a lost control plane can leave a host holding I/O.
//
// Every failure inside the window rolls back the same way: the control plane
// puts the namespaces back, every host that may hold I/O is resumed on the
// volume's source subsystem, and every host given the target's paths drops
// them.
//
// design-cg-live-subsystem-flip.md §6 and §7 are the specification.

package volume

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/statemachine"
	"github.com/simplyblock/atlas/volstack/swaprpc"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// volumeIDOf is the backend volume a PersistentVolume fronts.
func volumeIDOf(pv *corev1.PersistentVolume) (string, bool) {
	if pv.Spec.CSI == nil {
		return "", false
	}
	return lvolIDOf(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle))
}

// lvolIDOf is the backend volume a handle names.
func lvolIDOf(handle lvol.VolumeHandle) (string, bool) {
	parsed, ok := lvol.ParseHandle(handle)
	if !ok {
		return "", false
	}
	return parsed.VolumeID, true
}

// hostVolumes are the CSI handles of the moving volumes a host consumes, keyed
// by the subsystem each one leaves.
type hostVolumes struct {
	handles   []string
	bySource  map[string][]string
	claimOfPV map[string]string
	// pvOfHandle is the PersistentVolume each handle was read from.
	pvOfHandle map[string]string
}

// volumesOn reads what a host consumes from status and the volumes' objects.
func (r *PersistentVolumeOpsReconciler) volumesOn(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, host simplyblockv1alpha2.SwapHost,
) (hostVolumes, error) {
	sources := map[string]string{}
	for _, v := range joinOf(ops).Volumes {
		sources[v.PersistentVolumeName] = v.SourceNQN
	}
	out := hostVolumes{bySource: map[string][]string{}, claimOfPV: map[string]string{}, pvOfHandle: map[string]string{}}
	for _, name := range host.PersistentVolumes {
		var pv corev1.PersistentVolume
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &pv); err != nil {
			return hostVolumes{}, fmt.Errorf("read volume %s consumed on %s: %w", name, host.Node, err)
		}
		if pv.Spec.CSI == nil {
			continue
		}
		handle := pv.Spec.CSI.VolumeHandle
		out.handles = append(out.handles, handle)
		out.bySource[sources[name]] = append(out.bySource[sources[name]], handle)
		out.claimOfPV[handle] = claimOf(&pv)
		out.pvOfHandle[handle] = name
	}
	return out, nil
}

// unrecordedHosts is every host a moving volume is attached to now that
// status.hosts does not name: one that attached after the hosts were
// recorded at SelectingSubsystem. Attachment is a kubelet decision the
// operation does not stop, so the list is read again where it matters.
func (r *PersistentVolumeOpsReconciler) unrecordedHosts(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) ([]simplyblockv1alpha2.SwapHost, error) {
	fresh, err := r.consumingHosts(ctx, joinOf(ops).Volumes)
	if err != nil {
		return nil, err
	}
	var out []simplyblockv1alpha2.SwapHost
	for _, host := range fresh {
		if !slices.ContainsFunc(joinOf(ops).Hosts, func(h simplyblockv1alpha2.SwapHost) bool { return h.Node == host.Node }) {
			out = append(out, host)
		}
	}
	return out, nil
}

// stagedOn narrows a host's share of the move to the volumes it has staged,
// as its swap service reports them. A volume attached to the host but not
// staged there holds no path to swap: its stage is refused while the move
// exists, and it attaches to the target after the commit.
func (r *PersistentVolumeOpsReconciler) stagedOn(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, node SwapNode, host simplyblockv1alpha2.SwapHost,
) (simplyblockv1alpha2.SwapHost, error) {
	volumes, err := r.volumesOn(ctx, ops, host)
	if err != nil {
		return simplyblockv1alpha2.SwapHost{}, err
	}
	states, err := node.Describe(ctx, volumes.handles)
	if err != nil {
		return simplyblockv1alpha2.SwapHost{}, fmt.Errorf("describe the volumes on %s: %w", host.Node, err)
	}
	staged := host
	staged.PersistentVolumes = nil
	for _, state := range states {
		if state.Staged {
			staged.PersistentVolumes = append(staged.PersistentVolumes, volumes.pvOfHandle[state.Handle])
		}
	}
	sort.Strings(staged.PersistentVolumes)
	return staged, nil
}

// unrecordedConsumer is a host that attached a moving volume after the hosts
// were recorded and has it staged, or one that cannot say: the operation
// cannot hold that host's I/O, so it must not move the namespaces under it.
// Empty when every consumer is recorded.
func (r *PersistentVolumeOpsReconciler) unrecordedConsumer(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (string, error) {
	newcomers, err := r.unrecordedHosts(ctx, ops)
	if err != nil {
		return "", err
	}
	for _, host := range newcomers {
		node, _, err := r.reach(host.Node)
		if err != nil {
			return host.Node, nil
		}
		staged, err := r.stagedOn(ctx, ops, node, host)
		if err != nil {
			return host.Node, nil //nolint:nilerr // a host that cannot answer is one the swap cannot hold
		}
		if len(staged.PersistentVolumes) > 0 {
			return host.Node, nil
		}
	}
	return "", nil
}

// swapNode is the swap service on a host, or the rollback a host that cannot
// swap forces.
func (r *PersistentVolumeOpsReconciler) swapNode(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, host string,
) (SwapNode, string, error) {
	if r.Swap == nil {
		return nil, "", r.swapUnavailable(ctx, ops, subject,
			"the operator is not linked to any CSI node plugin, so host %s cannot hold the volume's I/O", host)
	}
	node, instance, err := r.reach(host)
	if errors.Is(err, ErrNoSwapPeer) || errors.Is(err, ErrNoSwapCapability) {
		return nil, "", r.swapUnavailable(ctx, ops, subject, "host %s cannot swap the volume: %v", host, err)
	}
	return node, instance, err
}

// swapUnavailable reports a host that cannot take part, and rolls back.
func (r *PersistentVolumeOpsReconciler) swapUnavailable(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, format string, args ...any,
) error {
	message := fmt.Sprintf(format, args...)
	r.event(ops, corev1.EventTypeWarning, ReasonSubsystemSwapUnavailable, "%s", message)
	r.claimEvent(ctx, subject.pv, corev1.EventTypeWarning, ReasonSubsystemSwapUnavailable, "%s", message)
	return rollbackf("%s", message)
}

// driverWithoutIndirection is the SimplyblockDriver that provisioned pv when it
// stages volumes without the device-mapper indirection, and nil otherwise. A
// volume such a driver stages is restaged the same way, so a pod restart alone
// does not make it swappable.
func (r *PersistentVolumeOpsReconciler) driverWithoutIndirection(
	ctx context.Context, pv *corev1.PersistentVolume,
) (*simplyblockv1alpha2.SimplyblockDriver, error) {
	if pv.Spec.CSI == nil {
		return nil, nil
	}
	var drivers simplyblockv1alpha2.SimplyblockDriverList
	if err := r.List(ctx, &drivers); err != nil {
		return nil, fmt.Errorf("list the CSI drivers this operator manages: %w", err)
	}
	for i := range drivers.Items {
		d := &drivers.Items[i]
		if d.Spec.DriverName == pv.Spec.CSI.Driver && d.Spec.DisableDeviceMapperIndirection {
			return d, nil
		}
	}
	return nil, nil
}

// updateHost applies a change to one host's record in status.
func (r *PersistentVolumeOpsReconciler) updateHost(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, node string,
	change func(*simplyblockv1alpha2.SwapHost),
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		join := joinStatus(status)
		for i := range join.Hosts {
			if join.Hosts[i].Node == node {
				change(&join.Hosts[i])
			}
		}
	})
}

// joinAttach confirms every host can swap every moving volume it consumes, and
// gives it the target's paths.
func (r *PersistentVolumeOpsReconciler) joinAttach(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	// A host that attached since the hosts were recorded and has a moving
	// volume staged is one more to prepare and hold: recorded here, it is
	// prepared by the pass that follows, like the others. One with nothing
	// staged holds no path and is not recorded; it is looked at again on the
	// next pass, in case it stages meanwhile.
	newcomers, err := r.unrecordedHosts(ctx, ops)
	if err != nil {
		return "", false, err
	}
	var consumers []simplyblockv1alpha2.SwapHost
	for _, host := range newcomers {
		node, _, err := r.swapNode(ctx, ops, subject, host.Node)
		if err != nil {
			return "", false, err
		}
		staged, err := r.stagedOn(ctx, ops, node, host)
		if err != nil {
			return "", false, err
		}
		if len(staged.PersistentVolumes) > 0 {
			consumers = append(consumers, staged)
		}
	}
	if len(consumers) > 0 {
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			join := joinStatus(status)
			join.Hosts = append(join.Hosts, consumers...)
			sort.Slice(join.Hosts, func(i, j int) bool { return join.Hosts[i].Node < join.Hosts[j].Node })
		}); err != nil {
			return "", false, err
		}
		return "", false, waitingf("host %s attached a moving volume since the hosts were recorded; it is prepared next",
			consumers[0].Node)
	}

	join := joinOf(ops)
	for _, host := range join.Hosts {
		if host.Prepared {
			continue
		}
		node, instance, err := r.swapNode(ctx, ops, subject, host.Node)
		if err != nil {
			return "", false, err
		}
		volumes, err := r.volumesOn(ctx, ops, host)
		if err != nil {
			return "", false, err
		}
		states, err := node.Describe(ctx, volumes.handles)
		if err != nil {
			return "", false, fmt.Errorf("describe the volumes on %s: %w", host.Node, err)
		}
		// A volume attached to the host but not staged there holds no path to
		// swap (see stagedOn). It is left out of this host's swap, and a host
		// with nothing staged leaves the list; the newcomer check above looks
		// at it again each pass, and records it if it stages after all.
		var unstaged []string
		for _, state := range states {
			if !state.Staged {
				unstaged = append(unstaged, volumes.pvOfHandle[state.Handle])
			}
		}
		if len(unstaged) > 0 {
			if err := r.leaveOut(ctx, ops, host.Node, unstaged); err != nil {
				return "", false, err
			}
			return "", false, waitingf("claim %s is attached to host %s but not staged there; it is left out of the swap",
				volumes.claimOfPV[handleOfPV(volumes, unstaged[0])], host.Node)
		}
		for _, state := range states {
			if state.Swappable {
				continue
			}
			driver, err := r.driverWithoutIndirection(ctx, subject.pv)
			if err != nil {
				return "", false, err
			}
			if driver != nil {
				return "", false, r.swapUnavailable(ctx, ops, subject,
					"claim %s on host %s was staged without the device-mapper layer the swap re-points, "+
						"and SimplyblockDriver %s/%s sets spec.disableDeviceMapperIndirection, so a restage "+
						"would leave it out again. Clear that field, restart the claim's pod so it is staged "+
						"again, then delete this operation to retry",
					volumes.claimOfPV[state.Handle], host.Node, driver.Namespace, driver.Name)
			}
			return "", false, r.swapUnavailable(ctx, ops, subject,
				"claim %s on host %s was staged without the device-mapper layer the swap re-points; "+
					"restart its pod so it is staged again, then delete this operation to retry",
				volumes.claimOfPV[state.Handle], host.Node)
		}
		// The connect entries are a host's own (its DHCHAP secret among them),
		// so the control plane answers them only for a named host, and each host
		// is prepared with its own.
		hostNQN, err := r.hostNQNOf(ctx, host.Node)
		if err != nil {
			return "", false, err
		}
		prepared, err := r.Join.PrepareSubsystemMove(ctx, subject.clusterUUID, moveGroupOf(join), join.MoveID, hostNQN)
		if err != nil {
			return "", false, err
		}
		paths := nvmeof.Targets(prepared.Target)
		if len(paths) == 0 {
			return "", false, fmt.Errorf("the control plane returned no path to %s for host %s (%s)",
				join.TargetNQN, host.Node, hostNQN)
		}
		if _, err := node.Prepare(ctx, volumes.handles, join.TargetNQN, paths); err != nil {
			return "", false, fmt.Errorf("connect %s to the target: %w", host.Node, err)
		}
		if err := r.updateHost(ctx, ops, host.Node, func(h *simplyblockv1alpha2.SwapHost) {
			h.Prepared = true
			h.InstanceUID = instance
		}); err != nil {
			return "", false, err
		}
	}
	return stepSuspending, true, nil
}

// leaveOut removes the named volumes from a host's share of the swap, and the
// host itself once nothing of it is left.
func (r *PersistentVolumeOpsReconciler) leaveOut(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, node string, pvs []string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		join := joinStatus(status)
		for i := range join.Hosts {
			if join.Hosts[i].Node != node {
				continue
			}
			join.Hosts[i].PersistentVolumes = slices.DeleteFunc(join.Hosts[i].PersistentVolumes,
				func(pv string) bool { return slices.Contains(pvs, pv) })
			if len(join.Hosts[i].PersistentVolumes) == 0 {
				join.Hosts = slices.Delete(join.Hosts, i, i+1)
			}
			return
		}
	})
}

// handleOfPV is the handle volumesOn read from the named volume.
func handleOfPV(volumes hostVolumes, pv string) string {
	for handle, name := range volumes.pvOfHandle {
		if name == pv {
			return handle
		}
	}
	return ""
}

// hostNQNOf is the NVMe host NQN a Kubernetes node connects with: the kernel
// default its CSI node plugin publishes on the Node, or, before the plugin has
// published one, the form derived from the Node's UID.
func (r *PersistentVolumeOpsReconciler) hostNQNOf(ctx context.Context, nodeName string) (string, error) {
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return "", fmt.Errorf("read node %s for its host NQN: %w", nodeName, err)
	}
	if v := strings.TrimSpace(node.Annotations[kube.AnnoNVMeHostNQN]); strings.HasPrefix(v, "nqn.") {
		return v, nil
	}
	if node.UID == "" {
		return "", fmt.Errorf("node %s publishes no host NQN and has no UID to derive one from", nodeName)
	}
	return nqn.Host(string(node.UID)), nil
}

// joinSuspend holds every moving volume's I/O on every host.
//
// The deadline is persisted before the first host is asked to hold, and a
// host is marked as held before it is asked. A pass that dies after either
// write leaves a record that the hold may have happened, so the rollback
// resumes that host whatever it answered.
func (r *PersistentVolumeOpsReconciler) joinSuspend(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (step, bool, error) {
	// Before the first host is held, and so before the window opens: a host
	// that attached and staged a moving volume since AttachingPaths holds a
	// path this operation has not prepared and cannot hold, so the namespaces
	// must not move under it.
	if !slices.ContainsFunc(joinOf(ops).Hosts, func(h simplyblockv1alpha2.SwapHost) bool { return h.Suspended }) {
		host, err := r.unrecordedConsumer(ctx, ops)
		if err != nil {
			return "", false, err
		}
		if host != "" {
			return "", false, rollbackf("host %s attached a moving volume after the hosts were recorded and holds a path the swap cannot hold", host)
		}
	}
	if err := r.ensureDeadline(ctx, ops); err != nil {
		return "", false, err
	}
	deadline := joinOf(ops).SuspendDeadline.Time
	for _, host := range joinOf(ops).Hosts {
		if host.Suspended {
			continue
		}
		if !r.now().Before(deadline) {
			return "", false, rollbackf("the suspend window closed before host %s held its I/O", host.Node)
		}
		node, instance, err := r.reach(host.Node)
		if err != nil {
			return "", false, rollbackf("host %s cannot be reached to hold its I/O: %v", host.Node, err)
		}
		volumes, err := r.volumesOn(ctx, ops, host)
		if err != nil {
			return "", false, err
		}
		if err := r.updateHost(ctx, ops, host.Node, func(h *simplyblockv1alpha2.SwapHost) {
			h.Suspended = true
			h.InstanceUID = instance
		}); err != nil {
			return "", false, err
		}
		if _, err := node.Suspend(ctx, volumes.handles, deadline); err != nil {
			return "", false, rollbackf("host %s could not hold its I/O: %v", host.Node, err)
		}
	}
	return stepMovingNamespaces, true, nil
}

// ensureDeadline persists the suspend window's end, once.
func (r *PersistentVolumeOpsReconciler) ensureDeadline(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) error {
	if joinOf(ops).SuspendDeadline != nil {
		return nil
	}
	deadline := metav1.NewTime(r.now().Add(suspendTimeout(ops)))
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		joinStatus(status).SuspendDeadline = &deadline
	})
}

// moveHappened reports whether the control plane has the namespaces in the
// target, and records it in status when it learns so. Status alone cannot
// answer: a pass that died between the move call and its status write leaves
// a move that happened and a status that says it did not (live 2026-11-10).
// The control plane is asked only while status does not already say target,
// so a pass inside the window costs one GET at most.
func (r *PersistentVolumeOpsReconciler) moveHappened(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (bool, error) {
	join := joinOf(ops)
	if join.MoveID == "" {
		return false, nil
	}
	if join.NamespacesAt == simplyblockv1alpha2.SubsystemSideTarget {
		return true, nil
	}
	move, err := r.Join.GetSubsystemMove(ctx, subject.clusterUUID, moveGroupOf(join), join.MoveID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("read the move %s: %w", join.MoveID, err)
	}
	if move.NamespacesAt != controlplane.NamespacesAtTarget {
		return false, nil
	}
	if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		joinStatus(status).NamespacesAt = simplyblockv1alpha2.SubsystemSideTarget
	}); err != nil {
		return false, err
	}
	return true, nil
}

// pastDeadline reports that the suspend window has closed: from then on a
// host's watchdog may have resumed it on its own, on whichever subsystem held
// the namespace, so the operation can no longer assume where a host's I/O is.
func (r *PersistentVolumeOpsReconciler) pastDeadline(ops *simplyblockv1alpha2.PersistentVolumeOps) bool {
	deadline := joinOf(ops).SuspendDeadline
	return deadline != nil && !r.now().Before(deadline.Time)
}

// joinMove confirms every host still holds its I/O under the same plugin
// process, then has the control plane move the namespaces.
func (r *PersistentVolumeOpsReconciler) joinMove(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	if err := r.ensureDeadline(ctx, ops); err != nil {
		return "", false, err
	}
	join := joinOf(ops)
	happened, err := r.moveHappened(ctx, ops, subject)
	if err != nil {
		return "", false, err
	}
	// A move that happened is not checked for or started again: the hosts
	// may already have been resumed on the target by their watchdogs, and
	// the only way from here is forward.
	if !happened {
		if host, err := r.unrecordedConsumer(ctx, ops); err != nil {
			return "", false, err
		} else if host != "" {
			return "", false, rollbackf("host %s attached a moving volume inside the window and holds a path the swap cannot hold", host)
		}
		for _, host := range join.Hosts {
			node, instance, err := r.reach(host.Node)
			if err != nil {
				return "", false, rollbackf("host %s cannot be reached before the move: %v", host.Node, err)
			}
			if instance != host.InstanceUID {
				return "", false, rollbackf("the CSI plugin on host %s restarted while it held the volume's I/O", host.Node)
			}
			volumes, err := r.volumesOn(ctx, ops, host)
			if err != nil {
				return "", false, err
			}
			states, err := node.Describe(ctx, volumes.handles)
			if err != nil {
				return "", false, rollbackf("host %s could not confirm it holds its I/O: %v", host.Node, err)
			}
			for _, state := range states {
				if !state.Suspended {
					return "", false, rollbackf("host %s no longer holds the I/O of claim %s",
						host.Node, volumes.claimOfPV[state.Handle])
				}
			}
		}

		_, err = r.Join.StartSubsystemMove(ctx, subject.clusterUUID, moveGroupOf(join), join.MoveID, join.SuspendDeadline.Time)
		switch {
		case errors.Is(err, controlplane.ErrDeadlinePassed), errors.Is(err, controlplane.ErrMoveFailed),
			errors.Is(err, controlplane.ErrMoveConflict):
			return "", false, rollbackf("the control plane did not move the namespaces: %v", err)
		case err != nil && !r.now().Before(join.SuspendDeadline.Time):
			return "", false, rollbackf("the suspend window closed with the move unconfirmed: %v", err)
		case err != nil:
			return "", false, err
		}
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			joinStatus(status).NamespacesAt = simplyblockv1alpha2.SubsystemSideTarget
		}); err != nil {
			return "", false, err
		}
	}
	if len(join.Hosts) == 0 {
		return stepJoinCommitting, true, nil
	}
	return stepJoinResuming, true, nil
}

// joinResume points every host that may hold I/O at the subsystem the
// namespaces are in and releases it: the target on the way forward, each
// volume's source on the way back. On the way back, the control plane puts the
// namespaces back first.
func (r *PersistentVolumeOpsReconciler) joinResume(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	join := joinOf(ops)
	back := join.RollbackReason != ""
	if back && join.NamespacesAt != simplyblockv1alpha2.SubsystemSideSource {
		if err := r.Join.RollbackSubsystemMove(ctx, subject.clusterUUID, moveGroupOf(join), join.MoveID); err != nil {
			return "", false, err
		}
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			joinStatus(status).NamespacesAt = simplyblockv1alpha2.SubsystemSideSource
		}); err != nil {
			return "", false, err
		}
	}

	for _, host := range joinOf(ops).Hosts {
		if !host.Suspended || host.Resumed {
			continue
		}
		node, _, err := r.reach(host.Node)
		if err != nil {
			if !back && !r.now().Before(join.SuspendDeadline.Time) {
				return "", false, rollbackf("host %s could not be reached to release its I/O: %v", host.Node, err)
			}
			return "", false, fmt.Errorf("reach host %s to release its I/O: %w", host.Node, err)
		}
		volumes, err := r.volumesOn(ctx, ops, host)
		if err != nil {
			return "", false, err
		}
		if err := r.resumeHost(ctx, node, volumes, join.TargetNQN, back); err != nil {
			if !back && (errors.Is(err, swaprpc.ErrNamespaceNotFound) || !r.now().Before(join.SuspendDeadline.Time)) {
				return "", false, rollbackf("host %s could not find the moved namespace: %v", host.Node, err)
			}
			return "", false, fmt.Errorf("release the I/O on %s: %w", host.Node, err)
		}
		if err := r.updateHost(ctx, ops, host.Node, func(h *simplyblockv1alpha2.SwapHost) { h.Resumed = true }); err != nil {
			return "", false, err
		}
		if !back {
			for _, v := range host.PersistentVolumes {
				r.claimEventOf(ctx, v, corev1.EventTypeNormal, ReasonSubsystemSwapCompleted,
					"Host %s held the volume's I/O while its namespace moved to subsystem %s", host.Node, join.TargetNQN)
			}
		}
	}

	if back {
		return stepReleasingPaths, true, nil
	}
	if start := join.SuspendDeadline; start != nil {
		cgSubsystemMoveWindow.WithLabelValues(subject.clusterUUID).
			Observe(r.now().Sub(start.Add(-suspendTimeout(ops))).Seconds())
	}
	return stepJoinCommitting, true, nil
}

// resumeHost resumes one host's volumes on the target, or each on its source.
func (r *PersistentVolumeOpsReconciler) resumeHost(
	ctx context.Context, node SwapNode, volumes hostVolumes, target string, back bool,
) error {
	if !back {
		_, err := node.Resume(ctx, volumes.handles, target)
		return err
	}
	sources := make([]string, 0, len(volumes.bySource))
	for source := range volumes.bySource {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		if _, err := node.Resume(ctx, volumes.bySource[source], source); err != nil {
			return err
		}
	}
	return nil
}

// joinRelease disconnects, on every host, the subsystem its volumes no longer
// use: their sources after a move, the target after a rollback.
func (r *PersistentVolumeOpsReconciler) joinRelease(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (step, bool, error) {
	join := joinOf(ops)
	back := join.RollbackReason != ""
	for _, host := range join.Hosts {
		if host.Released || (back && !host.Prepared) {
			continue
		}
		node, _, err := r.reach(host.Node)
		if err != nil {
			return "", false, fmt.Errorf("reach host %s to release the subsystem it left: %w", host.Node, err)
		}
		var nqns []string
		if back {
			nqns = []string{join.TargetNQN}
		} else {
			volumes, err := r.volumesOn(ctx, ops, host)
			if err != nil {
				return "", false, err
			}
			for source := range volumes.bySource {
				nqns = append(nqns, source)
			}
			sort.Strings(nqns)
		}
		for _, subsystem := range nqns {
			if _, err := node.Release(ctx, subsystem); err != nil {
				return "", false, fmt.Errorf("release subsystem %s on %s: %w", subsystem, host.Node, err)
			}
		}
		if err := r.updateHost(ctx, ops, host.Node, func(h *simplyblockv1alpha2.SwapHost) { h.Released = true }); err != nil {
			return "", false, err
		}
	}
	return afterSwap(join), true, nil
}

// beginRollback stops moving the group forward. The reason is recorded first,
// so every later pass, a restarted one included, continues the rollback rather
// than the move. Hosts that may hold I/O are resumed next, and a rollback with
// none goes straight to dropping the target's paths.
func (r *PersistentVolumeOpsReconciler) beginRollback(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
	machine *statemachine.Machine[step], reason string,
) (ctrl.Result, error) {
	// Once the window has closed, a move that happened is never taken back:
	// a host the operation cannot reach may have been resumed on the target
	// by its watchdog, and putting the namespaces back would fail its I/O.
	// The operation goes forward instead, resuming each host on the target as
	// it can reach it, and commits. Before the deadline the hosts are still
	// held, and a rollback is what the step asked for.
	if joinOf(ops).RollbackReason == "" && r.pastDeadline(ops) {
		happened, err := r.moveHappened(ctx, ops, subject)
		if err != nil {
			return ctrl.Result{RequeueAfter: joinInsideWindow}, r.note(ctx, ops,
				fmt.Sprintf("%s; could not learn from the control plane whether the move happened: %v", reason, err))
		}
		if happened {
			r.event(ops, corev1.EventTypeWarning, ReasonSubsystemSwapDeferred,
				"The move had happened when %s; the hosts are resumed on the target as they can be reached, and the move is committed", reason)
			return r.enterJoinStep(ctx, ops, machine, stepJoinResuming)
		}
	}
	if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		join := joinStatus(status)
		if join.RollbackReason == "" {
			join.RollbackReason = reason
		}
		for i := range join.Hosts {
			join.Hosts[i].Resumed = false
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.event(ops, corev1.EventTypeWarning, ReasonSubsystemSwapRolledBack, "Rolling the subsystem move back: %s", reason)
	for _, v := range joinOf(ops).Volumes {
		r.claimEventOf(ctx, v.PersistentVolumeName, corev1.EventTypeWarning, ReasonSubsystemSwapRolledBack,
			"The move into the group's subsystem is rolled back, and the volume stays in %s: %s", v.SourceNQN, reason)
	}

	held := false
	for _, host := range joinOf(ops).Hosts {
		held = held || host.Suspended
	}
	if !held {
		join := joinOf(ops)
		if err := r.Join.RollbackSubsystemMove(ctx, subject.clusterUUID, moveGroupOf(join), join.MoveID); err != nil {
			return ctrl.Result{RequeueAfter: joinInsideWindow}, r.note(ctx, ops,
				fmt.Sprintf("rolling back: %s; releasing the move: %v", reason, err))
		}
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			joinStatus(status).NamespacesAt = simplyblockv1alpha2.SubsystemSideSource
		}); err != nil {
			return ctrl.Result{}, err
		}
		if len(joinOf(ops).Hosts) == 0 {
			return r.finishJoin(ctx, ops, subject, "")
		}
		return r.enterJoinStep(ctx, ops, machine, stepReleasingPaths)
	}
	return r.enterJoinStep(ctx, ops, machine, stepJoinResuming)
}

// resumeEveryHost resumes, best effort, every host that may hold I/O, on the
// target or on each volume's source. It is the deletion path's, where an
// unreachable host is left to its own watchdog rather than holding the delete.
func (r *PersistentVolumeOpsReconciler) resumeEveryHost(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, toTarget bool,
) {
	join := joinOf(ops)
	for _, host := range join.Hosts {
		if !host.Suspended || host.Resumed {
			continue
		}
		node, _, err := r.reach(host.Node)
		if err == nil {
			var volumes hostVolumes
			volumes, err = r.volumesOn(ctx, ops, host)
			if err == nil {
				err = r.resumeHost(ctx, node, volumes, join.TargetNQN, !toTarget)
			}
		}
		if err != nil {
			logf.FromContext(ctx).Error(err, "a host could not be resumed; its watchdog resumes it", "node", host.Node)
		}
	}
}

// reach is the swap service on a host, for a step past the attach, where a
// host that cannot be reached is a retry or a rollback rather than a refusal.
func (r *PersistentVolumeOpsReconciler) reach(host string) (SwapNode, string, error) {
	if r.Swap == nil {
		return nil, "", fmt.Errorf("%w: the operator is not linked to the CSI node plugins", ErrNoSwapPeer)
	}
	return r.Swap.Node(host)
}

// claimEventOf records an event on the claim of the named volume.
func (r *PersistentVolumeOpsReconciler) claimEventOf(
	ctx context.Context, pvName, eventType, reason, format string, args ...any,
) {
	if pvName == "" {
		return
	}
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: pvName}, &pv); err != nil {
		return
	}
	r.claimEvent(ctx, &pv, eventType, reason, format, args...)
}
