// The HostMaintenance window's steps that talk to FoundationDB, each through
// one command Job of fdbcommand.go: entering and leaving maintenance mode for
// the worker's zone, moving the coordinators off the worker and verifying the
// move in the cluster file, excluding the worker's log and stateless processes
// so their roles move while they are alive and including them once they are
// back, and waiting for the worker's process groups to report back.
//
// None of these steps holds the drain on a failed command. A command that
// fails completes its step degraded, with an event saying what did not happen,
// because the budget's job is to hold the eviction until the move has been
// attempted, and an attempt that failed is an attempt.
//
// design-fdb-host-maintenance.md §5.5 through §5.9 and §5.12 are the
// specification.

package controlplane

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// maintenanceZoneLine is what the bare `maintenance` command prints while a
// zone is under maintenance. The other answer is "No ongoing maintenance."
var maintenanceZoneLine = regexp.MustCompile(`Maintenance for zone (\S+) will continue`)

// windowEnterMaintenance is the EnteringMaintenance step (§5.7). It sets
// FoundationDB's maintenance mode for the zone for the length of the
// AwaitingHost budget, which is what stops data distribution from copying the
// host's storage data away while the host is absent.
func (r *ControlPlaneOpsReconciler) windowEnterMaintenance(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	zone := ops.Status.HostMaintenance.Zone
	seconds := int(awaitingHostDeadline.Seconds())
	job, err := r.commandJob(ctx, ops, target, fdbCommandMaintenanceOn,
		fmt.Sprintf("maintenance on %s %d", zone, seconds))
	if err != nil {
		return false, "", err
	}
	switch jobOutcome(job) {
	case jobRunning:
		return false, fmt.Sprintf("setting maintenance mode for zone %s", zone), nil
	case jobFailed:
		r.emit(ops, corev1.EventTypeWarning, MaintenanceModeNotSet, fmt.Sprintf(
			"maintenance mode could not be set for zone %s (Job %s failed); the window continues "+
				"without it, and data distribution may copy the host's data while it is away", zone, job.Name))
		return true, "", nil
	}

	until := metav1.NewTime(time.Now().Add(time.Duration(seconds) * time.Second))
	if job.Status.CompletionTime != nil {
		until = metav1.NewTime(job.Status.CompletionTime.Add(time.Duration(seconds) * time.Second))
	}
	if err := r.patchWindow(ctx, ops, func(s *simplyblockv1alpha2.HostMaintenanceStatus) {
		s.MaintenanceUntil = &until
	}); err != nil {
		return false, "", err
	}
	r.emit(ops, corev1.EventTypeNormal, MaintenanceModeSet,
		fmt.Sprintf("maintenance mode is set for zone %s until %s", zone, until.UTC().Format(time.RFC3339)))
	fdbMaintenanceZoneActive.WithLabelValues(ops.Namespace, ops.Spec.HostMaintenance.WorkerNode).Set(1)

	// FoundationDB holds one zone, so this window's took over whatever zone an
	// earlier window still had. That window learns it here, and its own
	// LeavingMaintenance will find the zone is not its to clear.
	others, err := r.otherWindows(ctx, ops)
	if err != nil {
		return false, "", err
	}
	for i := range others {
		other := &others[i]
		hm := other.Status.HostMaintenance
		if hm == nil || hm.MaintenanceUntil == nil || hm.Zone == zone {
			continue
		}
		r.emit(other, corev1.EventTypeNormal, MaintenanceZoneTakenOver, fmt.Sprintf(
			"window %s set maintenance mode for zone %s, which ended this window's zone %s: "+
				"FoundationDB holds one maintenance zone", ops.Name, zone, hm.Zone))
	}
	return true, "", nil
}

// windowMoveCoordinators is the MovingCoordinators step (§5.5, §5.6). It
// chooses the set, writes it to status before the Job that writes it to the
// database, and completes once the cluster file names no process on the host,
// or once the window has recorded that it writes nothing.
func (r *ControlPlaneOpsReconciler) windowMoveCoordinators(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	hm := ops.Status.HostMaintenance
	zone := hm.Zone

	if hm.CoordinatorPlacement == "" {
		choice, err := r.chooseForWindow(ctx, ops)
		if err != nil {
			return false, "", err
		}
		if err := r.patchWindow(ctx, ops, func(s *simplyblockv1alpha2.HostMaintenanceStatus) {
			s.Coordinators = choice.Set
			s.CoordinatorPlacement = choice.Placement
		}); err != nil {
			return false, "", err
		}
		if choice.Placement == simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementUnchanged {
			fdbCoordinatorMovesTotal.WithLabelValues(ops.Namespace, string(choice.Placement)).Inc()
			if choice.Wanted > 0 {
				r.emit(ops, corev1.EventTypeWarning, CoordinatorsNotMoved, fmt.Sprintf(
					"the coordinators on zone %s were not moved: %d slots to fill and %d stateful "+
						"processes off the leaving hosts to fill them with; the host's absence runs "+
						"that many coordinators short", zone, choice.Wanted, choice.Available))
			}
			return true, "", nil
		}
		hm = ops.Status.HostMaintenance
	}
	if hm.CoordinatorPlacement == simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementUnchanged {
		return true, "", nil
	}

	job, err := r.commandJob(ctx, ops, target, fdbCommandCoordinators,
		"coordinators "+strings.Join(hm.Coordinators, " "))
	if err != nil {
		return false, "", err
	}
	switch jobOutcome(job) {
	case jobRunning:
		return false, fmt.Sprintf("moving the coordinators off zone %s", zone), nil
	case jobFailed:
		if err := r.patchWindow(ctx, ops, func(s *simplyblockv1alpha2.HostMaintenanceStatus) {
			s.CoordinatorPlacement = simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementUnchanged
		}); err != nil {
			return false, "", err
		}
		r.emit(ops, corev1.EventTypeWarning, CoordinatorsNotMoved, fmt.Sprintf(
			"the coordinators on zone %s were not moved: Job %s failed; the host's absence runs "+
				"one coordinator short", zone, job.Name))
		fdbCoordinatorMovesTotal.WithLabelValues(ops.Namespace,
			string(simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementUnchanged)).Inc()
		return true, "", nil
	}

	onHost, err := r.clusterFileNamesZone(ctx, ops.Namespace, zone)
	if err != nil {
		return false, "", err
	}
	if onHost {
		message := fmt.Sprintf("the cluster file still names a process on zone %s after the move completed", zone)
		if ops.Status.Message != message {
			r.emit(ops, corev1.EventTypeWarning, CoordinatorsReverted, message+
				": the FoundationDB operator reverted the set, or has not rewritten the file yet")
		}
		return false, message, nil
	}
	placement := "one per host"
	if hm.CoordinatorPlacement == simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementSharedHost {
		placement = "two sharing a host, because fewer hosts than coordinators remain off the leaving workers"
	}
	r.emit(ops, corev1.EventTypeNormal, CoordinatorsMoved, fmt.Sprintf(
		"the coordinators are %s, %s", strings.Join(hm.Coordinators, " "), placement))
	fdbCoordinatorMovesTotal.WithLabelValues(ops.Namespace, string(hm.CoordinatorPlacement)).Inc()
	return true, "", nil
}

// localityPrefix is how FoundationDB names a process by its instance ID in
// exclude and include, which is stable across the pod's IP changing when the
// eviction recreates it.
const localityPrefix = "locality_instance_id:"

// excludeCommand and includeCommand are the two commands of the hand-off over
// a list of process group IDs.
func excludeCommand(ids []string) string { return localityCommand("exclude", ids) }
func includeCommand(ids []string) string { return localityCommand("include", ids) }

func localityCommand(verb string, ids []string) string {
	parts := make([]string, 0, len(ids)+1)
	parts = append(parts, verb)
	for _, id := range ids {
		parts = append(parts, localityPrefix+id)
	}
	return strings.Join(parts, " ")
}

// handoffCandidates are the recorded process groups whose class is not
// storage, in the recorded order: the ones whose roles an exclusion moves
// without moving data.
func handoffCandidates(recorded []string, groups []fdbProcessGroup) []string {
	classOf := make(map[string]string, len(groups))
	for _, g := range groups {
		classOf[g.ID] = g.Class
	}
	var ids []string
	for _, id := range recorded {
		class, known := classOf[id]
		if known && class != "storage" {
			ids = append(ids, id)
		}
	}
	return ids
}

// windowExcludeProcesses is the ExcludingProcesses step (§5.12). It records
// the worker's log and stateless process groups before the Job that excludes
// them, issues the waiting exclude so that the Job completes only once
// FoundationDB reports the processes safe to remove, and completes on either
// outcome: a failed exclusion is reported and the budget still comes down,
// and the recorded list is kept because the exclusion may have been written
// before the wait timed out.
func (r *ControlPlaneOpsReconciler) windowExcludeProcesses(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	hm := ops.Status.HostMaintenance
	zone := hm.Zone
	if len(hm.ExcludedProcesses) == 0 {
		groups, err := r.fdbProcessGroups(ctx, ops.Namespace)
		if err != nil {
			return false, "", err
		}
		ids := handoffCandidates(hm.ProcessGroups, groups)
		if len(ids) == 0 {
			return true, "", nil
		}
		if err := r.patchWindow(ctx, ops, func(s *simplyblockv1alpha2.HostMaintenanceStatus) {
			s.ExcludedProcesses = ids
		}); err != nil {
			return false, "", err
		}
		hm = ops.Status.HostMaintenance
	}

	job, err := r.commandJob(ctx, ops, target, fdbCommandExclude, excludeCommand(hm.ExcludedProcesses))
	if err != nil {
		return false, "", err
	}
	switch jobOutcome(job) {
	case jobRunning:
		return false, fmt.Sprintf("handing the roles of %s off zone %s",
			strings.Join(hm.ExcludedProcesses, ", "), zone), nil
	case jobFailed:
		r.emit(ops, corev1.EventTypeWarning, ProcessesNotHandedOff, fmt.Sprintf(
			"the roles of %s were not handed off zone %s (Job %s failed); the eviction proceeds with "+
				"them in place and costs the recovery the hand-off spares",
			strings.Join(hm.ExcludedProcesses, ", "), zone, job.Name))
		return true, "", nil
	}
	r.emit(ops, corev1.EventTypeNormal, ProcessesHandedOff, fmt.Sprintf(
		"%s are excluded and hold no role; the eviction of zone %s kills nothing the transaction system needs",
		strings.Join(hm.ExcludedProcesses, ", "), zone))
	return true, "", nil
}

// windowIncludeProcesses is the IncludingProcesses step (§5.12). It lifts the
// exclusion the window wrote, once the processes are back to take roles
// again, and completes on either outcome: a failed include hands the command
// to a human, because a process left excluded runs no role.
func (r *ControlPlaneOpsReconciler) windowIncludeProcesses(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	hm := ops.Status.HostMaintenance
	if len(hm.ExcludedProcesses) == 0 {
		return true, "", nil
	}
	command := includeCommand(hm.ExcludedProcesses)
	job, err := r.commandJob(ctx, ops, target, fdbCommandInclude, command)
	if err != nil {
		return false, "", err
	}
	switch jobOutcome(job) {
	case jobRunning:
		return false, fmt.Sprintf("including %s again", strings.Join(hm.ExcludedProcesses, ", ")), nil
	case jobFailed:
		r.emit(ops, corev1.EventTypeWarning, ProcessesLeftExcluded, fmt.Sprintf(
			"%s are still excluded and run no role (Job %s failed); run `%s` against the database by hand",
			strings.Join(hm.ExcludedProcesses, ", "), job.Name, command))
		return true, "", nil
	}
	r.emit(ops, corev1.EventTypeNormal, ProcessesIncluded,
		fmt.Sprintf("%s are included again and may take roles", strings.Join(hm.ExcludedProcesses, ", ")))
	return true, "", nil
}

// windowAwaitHost is the AwaitingHost step (§5.9). It completes when every
// process group recorded at Holding is back: its pod is ready, carries no
// MissingProcesses condition, and is either a replacement the drain's eviction
// produced, which is a pod younger than the drain, or the pod from before the
// drain on a worker that is schedulable again, which is a cordon that was
// never drained. It reads nothing else from the Node: the processes come back
// on the same node after a reboot, on a replacement after a termination, and
// a group that left the status altogether was replaced by the FoundationDB
// operator and counts as returned.
//
// The eviction's effect takes seconds to show in the FoundationDB operator's
// status, so the pods are what the step reads first: the lab's first drain
// ended this step nine seconds before MissingProcesses was written.
func (r *ControlPlaneOpsReconciler) windowAwaitHost(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) (bool, string, error) {
	hm := ops.Status.HostMaintenance
	worker := ops.Spec.HostMaintenance.WorkerNode

	groups, err := r.fdbProcessGroups(ctx, ops.Namespace)
	if err != nil {
		return false, "", err
	}
	byID := make(map[string]fdbProcessGroup, len(groups))
	for _, g := range groups {
		byID[g.ID] = g
	}
	var pods corev1.PodList
	err = r.List(ctx, &pods, client.InNamespace(ops.Namespace), client.HasLabels{utils.LabelFDBClusterName})
	if err != nil {
		return false, "", fmt.Errorf("list the FoundationDB pods: %w", err)
	}
	podOf := make(map[string]*corev1.Pod, len(pods.Items))
	for i := range pods.Items {
		podOf[pods.Items[i].Labels["foundationdb.org/fdb-process-group-id"]] = &pods.Items[i]
	}
	schedulable := false
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: worker}, &node); err == nil {
		schedulable = !node.Spec.Unschedulable
	} else if !apierrors.IsNotFound(err) {
		return false, "", fmt.Errorf("read Node %s: %w", worker, err)
	}

	var waiting []string
	for _, id := range hm.ProcessGroups {
		g, inStatus := byID[id]
		if !inStatus {
			continue
		}
		pod := podOf[id]
		switch {
		case pod == nil:
			waiting = append(waiting, id+" (no pod)")
		case g.Missing:
			waiting = append(waiting, id+" (missing)")
		case !podReady(pod):
			waiting = append(waiting, id+" (pod not ready)")
		case hm.DrainObservedAt != nil && pod.CreationTimestamp.After(hm.DrainObservedAt.Time):
			// A replacement the eviction produced, ready: back.
		case schedulable:
			// The pod from before the drain, on a worker schedulable again: the
			// cordon was never drained, and the processes never left.
		default:
			waiting = append(waiting, id+" (not yet evicted)")
		}
	}
	if len(waiting) == 0 {
		return true, "", nil
	}
	message := fmt.Sprintf("waiting for %d FoundationDB process groups of %s to report back: %s",
		len(waiting), worker, strings.Join(waiting, ", "))
	if ops.Status.Message != message {
		r.emit(ops, corev1.EventTypeNormal, AwaitingHostReturn, message)
	}
	return false, message, nil
}

// podReady is a running pod whose containers are all ready.
func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning || len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, c := range pod.Status.ContainerStatuses {
		if !c.Ready {
			return false
		}
	}
	return true
}

// windowLeaveMaintenance is the LeavingMaintenance step (§5.7). It reads which
// zone is under maintenance and clears it only when it is the window's own. A
// window never clears a zone it did not set, and a window that set none has
// nothing to do.
func (r *ControlPlaneOpsReconciler) windowLeaveMaintenance(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	hm := ops.Status.HostMaintenance
	// Whatever this step finds, the window stops believing it holds the zone
	// once the step is over: the gauge reads the window's belief, and a zone
	// another window took over or one that expired is no longer this one's.
	defer fdbMaintenanceZoneActive.WithLabelValues(ops.Namespace, ops.Spec.HostMaintenance.WorkerNode).Set(0)
	if hm.MaintenanceUntil == nil {
		return true, "", nil
	}
	zone := hm.Zone

	status, err := r.commandJob(ctx, ops, target, fdbCommandMaintenanceStatus, "maintenance")
	if err != nil {
		return false, "", err
	}
	switch jobOutcome(status) {
	case jobRunning:
		return false, "reading which zone is under maintenance", nil
	case jobFailed:
		r.emit(ops, corev1.EventTypeWarning, MaintenanceModeLeft, fmt.Sprintf(
			"the maintenance zone could not be read (Job %s failed); zone %s expires on its own at %s",
			status.Name, zone, hm.MaintenanceUntil.UTC().Format(time.RFC3339)))
		return true, "", nil
	}
	answer, err := jobTerminationMessage(ctx, r.Client, status)
	if err != nil {
		return false, "", err
	}
	active, ok := maintenanceZoneOf(answer)
	switch {
	case !ok:
		r.emit(ops, corev1.EventTypeWarning, MaintenanceModeLeft, fmt.Sprintf(
			"the maintenance status %q could not be read; zone %s expires on its own at %s",
			strings.TrimSpace(answer), zone, hm.MaintenanceUntil.UTC().Format(time.RFC3339)))
		return true, "", nil
	case active != zone:
		// Another window's zone, or none: not this window's to clear.
		return true, "", nil
	}

	off, err := r.commandJob(ctx, ops, target, fdbCommandMaintenanceOff, "maintenance off")
	if err != nil {
		return false, "", err
	}
	switch jobOutcome(off) {
	case jobRunning:
		return false, fmt.Sprintf("clearing maintenance mode for zone %s", zone), nil
	case jobFailed:
		r.emit(ops, corev1.EventTypeWarning, MaintenanceModeLeft, fmt.Sprintf(
			"maintenance mode could not be cleared (Job %s failed); zone %s expires on its own at %s",
			off.Name, zone, hm.MaintenanceUntil.UTC().Format(time.RFC3339)))
		return true, "", nil
	}
	r.emit(ops, corev1.EventTypeNormal, MaintenanceModeCleared,
		fmt.Sprintf("maintenance mode is cleared for zone %s", zone))
	return true, "", nil
}

// maintenanceZoneOf reads the bare `maintenance` command's answer: the zone
// under maintenance, "" for none, and false for an answer it does not know.
func maintenanceZoneOf(answer string) (string, bool) {
	if m := maintenanceZoneLine.FindStringSubmatch(answer); m != nil {
		return m[1], true
	}
	if strings.Contains(answer, "No ongoing maintenance") {
		return "", true
	}
	return "", false
}

// commandJob creates the window's Job for a command when it is not there, and
// returns it as it is.
func (r *ControlPlaneOpsReconciler) commandJob(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
	cmd fdbCommand,
	exec string,
) (*batchv1.Job, error) {
	sole, err := r.workerIsTheOnlyNode(ctx, target, ops.Spec.HostMaintenance.WorkerNode)
	if err != nil {
		return nil, err
	}
	job, err := ensureJob(ctx, r.Client, fdbCommandJob(ops, target, cmd, exec, sole))
	if err != nil {
		return nil, err
	}
	r.meterCommand(ctx, job, cmd)
	return job, nil
}

// meteredAnnotation marks a command Job whose duration has been observed, so a
// step that reads a finished Job on several passes observes it once.
const meteredAnnotation = "storage.simplyblock.io/metered"

// meterCommand observes a finished command Job's duration, once.
func (r *ControlPlaneOpsReconciler) meterCommand(ctx context.Context, job *batchv1.Job, cmd fdbCommand) {
	outcome := jobOutcome(job)
	if outcome == jobRunning || job.Annotations[meteredAnnotation] != "" ||
		job.Status.StartTime == nil || job.Status.CompletionTime == nil && outcome == jobSucceeded {
		return
	}
	end := time.Now()
	if job.Status.CompletionTime != nil {
		end = job.Status.CompletionTime.Time
	}
	result := "Complete"
	if outcome == jobFailed {
		result = "Failed"
	}
	fdbCommandDurationSeconds.WithLabelValues(job.Namespace, string(cmd), result).
		Observe(end.Sub(job.Status.StartTime.Time).Seconds())
	patch := client.MergeFrom(job.DeepCopy())
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[meteredAnnotation] = end.UTC().Format(time.RFC3339)
	// A failed annotation costs a duplicate observation on the next pass, and
	// nothing else; it is not worth failing the step for.
	_ = r.Patch(ctx, job, patch)
}

// workerIsTheOnlyNode reports whether no node other than the worker matches
// the database's selector, in which case the command Job has nowhere else to
// run and its anti-affinity is dropped.
func (r *ControlPlaneOpsReconciler) workerIsTheOnlyNode(
	ctx context.Context, target *simplyblockv1alpha2.ControlPlane, worker string,
) (bool, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, client.MatchingLabels(foundationDBNodeSelector(target))); err != nil {
		return false, fmt.Errorf("list the nodes the database may run on: %w", err)
	}
	for i := range nodes.Items {
		if nodes.Items[i].Name != worker {
			return false, nil
		}
	}
	return true, nil
}

// chooseForWindow runs the coordinator choice over what the cluster reports,
// with every other window that has observed its drain as a leaving zone.
func (r *ControlPlaneOpsReconciler) chooseForWindow(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) (coordinatorChoice, error) {
	file, err := r.clusterFile(ctx, ops.Namespace)
	if err != nil {
		return coordinatorChoice{}, err
	}
	groups, err := r.fdbProcessGroups(ctx, ops.Namespace)
	if err != nil {
		return coordinatorChoice{}, err
	}
	others, err := r.otherWindows(ctx, ops)
	if err != nil {
		return coordinatorChoice{}, err
	}
	leaving := map[string]bool{}
	for i := range others {
		hm := others[i].Status.HostMaintenance
		if hm != nil && hm.DrainObservedAt != nil && hm.Zone != "" {
			leaving[hm.Zone] = true
		}
	}
	return chooseCoordinators(coordinatorInput{
		ClusterFile:  file,
		Groups:       groups,
		Zone:         ops.Status.HostMaintenance.Zone,
		LeavingZones: leaving,
		ClusterName:  ComponentFDBCluster,
		Namespace:    ops.Namespace,
		DNSDomain:    utils.ClusterDNSDomain(),
	})
}

// clusterFile is the current content of the cluster-file ConfigMap key, which
// the FoundationDB operator rewrites after a coordinator change.
func (r *ControlPlaneOpsReconciler) clusterFile(ctx context.Context, namespace string) (string, error) {
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: namespace, Name: clusterFileConfigMapName}
	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("read the cluster file ConfigMap %s: %w", clusterFileConfigMapName, err)
	}
	return cm.Data[clusterFileKey], nil
}

// clusterFileNamesZone reports whether the cluster file still names a process
// group whose fault domain is the zone.
func (r *ControlPlaneOpsReconciler) clusterFileNamesZone(
	ctx context.Context, namespace, zone string,
) (bool, error) {
	file, err := r.clusterFile(ctx, namespace)
	if err != nil {
		return false, err
	}
	entries, _, err := parseClusterFile(file)
	if err != nil {
		return false, err
	}
	groups, err := r.fdbProcessGroups(ctx, namespace)
	if err != nil {
		return false, err
	}
	in := coordinatorInput{ClusterName: ComponentFDBCluster, Namespace: namespace, DNSDomain: utils.ClusterDNSDomain()}
	onZone := map[string]bool{}
	for _, g := range groups {
		if g.FaultDomain == zone {
			onZone[in.hostname(g.ID)] = true
		}
	}
	for _, e := range entries {
		if onZone[e.host] {
			return true, nil
		}
	}
	return false, nil
}

// otherWindows are the other HostMaintenance windows in the namespace that
// have not ended.
func (r *ControlPlaneOpsReconciler) otherWindows(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) ([]simplyblockv1alpha2.ControlPlaneOps, error) {
	var list simplyblockv1alpha2.ControlPlaneOpsList
	if err := r.List(ctx, &list, client.InNamespace(ops.Namespace)); err != nil {
		return nil, fmt.Errorf("list the operations in %s: %w", ops.Namespace, err)
	}
	var others []simplyblockv1alpha2.ControlPlaneOps
	for i := range list.Items {
		other := list.Items[i]
		if other.Name == ops.Name ||
			other.Spec.Action != simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance ||
			terminalOps(other.Status.Phase) {
			continue
		}
		others = append(others, other)
	}
	return others, nil
}
