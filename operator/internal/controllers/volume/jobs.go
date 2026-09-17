// The host-side half of a migration: which nodes have to take part, and the
// Jobs that check, release, and clear their NVMe-oF paths.
//
// A migration moves an NVMe-oF subsystem rather than one volume inside it, so
// every volume on that subsystem moves at once and every host consuming one of
// them is affected at the same instant. Checking only the host of the volume
// the operation names would leave every sibling's consumer pointing at the
// source, and at cutover those hosts lose their volume.
//
// The work runs in a Job rather than here because it is host work: it reads the
// node's own NVMe fabric and connects paths on it, and neither is visible from
// the operator's pod.
//
// design-persistentvolumeops.md §5 is the specification.

package volume

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

const (
	// migrationCtrlLossTimeout is how long the kernel keeps retrying a
	// migration target path whose controller it has lost. It is the CSI
	// driver's value rather than the control plane's, because a target path
	// becomes the volume's data path at cutover and one volume's paths must not
	// sit on two different timeouts depending on which of them last moved.
	migrationCtrlLossTimeout = vmigration.CtrlLossTmoSec

	// validationJobDeadline caps a validation Job's whole life, scheduling and
	// image pull included. Its purpose is to turn a Job that can never finish —
	// an unschedulable pod, a node that is not ready — into a failure rather
	// than a step parked forever. The check itself needs seconds.
	validationJobDeadline = 180

	// jobTTL is how long a finished Job is kept. Long enough to read, short
	// enough that a drain's worth of them does not accumulate.
	jobTTL = 3600

	// consumerNotRunning is the marker a consumer lookup puts in its error when
	// a pod references the claim and is not Running yet. It is a substring
	// rather than a sentinel because the message names the claim, and what the
	// caller needs is to tell "wait" from "failed."
	consumerNotRunning = "is not running yet"
)

// The three modes a host-side Job runs in.
const (
	modeValidate = "validate-migration"
	modeRelease  = "release-migration-paths"
)

// consumingNodes returns every worker node that consumes a volume of the
// migrated subsystem, sorted so the Job set is stable across reconciles.
//
// Membership comes from the control plane and is mapped back to
// PersistentVolumes through the CSI handle, then to consumers through the
// claim. A member whose volume or consumer cannot be found contributes no node;
// a member with a consumer that is not Running yet stops the whole lookup, so
// the caller waits rather than validating a set it knows to be incomplete.
func (r *PersistentVolumeOpsReconciler) consumingNodes(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) ([]string, error) {
	nqn := ops.Status.Migration.SubsystemNQN
	members, err := r.API.SubsystemVolumes(ctx, subject.clusterUUID, nqn)
	if err != nil {
		return nil, fmt.Errorf("list the volumes of subsystem %s: %w", nqn, err)
	}
	wanted := expectedMembers(members, subject.handle.VolumeID)

	volumes, err := r.volumesFronting(ctx, wanted)
	if err != nil {
		return nil, err
	}

	nodes := map[string]struct{}{}
	for _, pv := range volumes {
		node, err := r.consumerNodeOf(ctx, pv)
		if err != nil {
			return nil, err
		}
		if node != "" {
			nodes[node] = struct{}{}
		}
	}

	out := make([]string, 0, len(nodes))
	for node := range nodes {
		out = append(out, node)
	}
	sort.Strings(out)
	return out, nil
}

// volumesFronting maps backend volume UUIDs to the PersistentVolumes that front
// them. A member with no PersistentVolume is not consumed through this
// cluster's driver, so it has no host paths to check.
func (r *PersistentVolumeOpsReconciler) volumesFronting(
	ctx context.Context, volumeUUIDs map[string]struct{},
) ([]*corev1.PersistentVolume, error) {
	var list corev1.PersistentVolumeList
	if err := r.Reader.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list the persistent volumes: %w", err)
	}
	var out []*corev1.PersistentVolume
	for i := range list.Items {
		pv := &list.Items[i]
		if pv.Spec.CSI == nil {
			continue
		}
		handle, ok := lvol.ParseHandle(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle))
		if !ok {
			continue
		}
		if _, wanted := volumeUUIDs[handle.VolumeID]; wanted {
			out = append(out, pv)
		}
	}
	return out, nil
}

// consumerNodeOf returns the node running a pod that mounts this volume's
// claim, the empty string when nothing consumes it, and an error naming
// consumerNotRunning when a pod references the claim and has not started.
//
// The three answers are distinct because the caller does something different
// with each. A volume nothing consumes has no host paths to check. A volume
// whose consumer is coming has to be waited for. Everything else is a lookup
// that failed and is retried.
//
// The reads go through the uncached reader: a stale cache can miss a running
// consumer, and a missed consumer is a host that never gets the target's paths.
func (r *PersistentVolumeOpsReconciler) consumerNodeOf(
	ctx context.Context, pv *corev1.PersistentVolume,
) (string, error) {
	if pv.Spec.ClaimRef == nil {
		return "", nil
	}
	claim, namespace := pv.Spec.ClaimRef.Name, pv.Spec.ClaimRef.Namespace

	var pods corev1.PodList
	if err := r.Reader.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list the pods in namespace %s: %w", namespace, err)
	}

	referenced := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !mounts(pod, claim) {
			continue
		}
		referenced = true
		if pod.Spec.NodeName != "" && pod.Status.Phase == corev1.PodRunning {
			return pod.Spec.NodeName, nil
		}
	}
	if referenced {
		return "", fmt.Errorf("a consumer of claim %s/%s %s", namespace, claim, consumerNotRunning)
	}
	return "", nil
}

// mounts reports whether the pod has this claim among its volumes.
func mounts(pod *corev1.Pod, claim string) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

// startValidationJobs creates a Job on every consuming node that has none yet,
// and returns how many it started.
//
// Existing entries are kept, so it also serves the re-check before the cutover:
// the control plane lets a volume join the subsystem until the migration is
// activated, and a consumer can be rescheduled while the checks run, so a node
// can appear that was not there when the first round started.
func (r *PersistentVolumeOpsReconciler) startValidationJobs(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	subject *subject,
	nodes []string,
) (int, error) {
	have := map[string]struct{}{}
	for _, job := range ops.Status.Migration.ValidationJobs {
		have[job.Node] = struct{}{}
	}

	var missing []string
	for _, node := range nodes {
		if _, ok := have[node]; !ok {
			missing = append(missing, node)
		}
	}
	if len(missing) == 0 {
		return 0, nil
	}

	image, err := vmigration.JobImage(ctx, r.Client, subject.namespace(), subject.clusterUUID)
	if err != nil {
		return 0, err
	}

	started := make([]simplyblockv1alpha2.ValidationJob, 0, len(missing))
	for _, node := range missing {
		job := r.pathJob(ops, subject, node, image, modeValidate)
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return 0, fmt.Errorf("start the validation on node %s: %w", node, err)
		}
		started = append(started, simplyblockv1alpha2.ValidationJob{
			Namespace: job.Namespace,
			Name:      job.Name,
			Node:      node,
		})
	}

	if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Migration.ValidationJobs = append(status.Migration.ValidationJobs, started...)
	}); err != nil {
		return 0, err
	}

	r.event(ops, corev1.EventTypeNormal, ReasonValidationStarted,
		"Checking the target's paths for subsystem %s on %s",
		ops.Status.Migration.SubsystemNQN, strings.Join(missing, ", "))
	return len(started), nil
}

// validationJobsPassed reports whether every node's check has succeeded.
//
// The first failure is fatal to the operation rather than retried. A cutover is
// subsystem-wide, so continuing with a subset of the hosts ready guarantees an
// outage for the rest, and retrying the check would only delay that decision
// behind a second run of a check that just said no.
func (r *PersistentVolumeOpsReconciler) validationJobsPassed(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (bool, error) {
	passed := false
	pending := 0

	jobs := ops.Status.Migration.ValidationJobs
	for i := range jobs {
		record := &jobs[i]
		// A node whose check already passed is not looked at again: its Job is
		// left for its own TTL to reap, and re-reading a reaped Job would look
		// like a node that was never checked.
		if record.Succeeded {
			continue
		}

		var job batchv1.Job
		err := r.Get(ctx, types.NamespacedName{Namespace: record.Namespace, Name: record.Name}, &job)
		switch {
		case apierrors.IsNotFound(err):
			// The Job went before a terminal state was observed — an eviction,
			// or somebody deleting it. Forget it so the next pass starts that
			// node over rather than waiting on a Job that no longer exists.
			return false, r.forgetValidationJob(ctx, ops, record.Name)
		case err != nil:
			return false, fmt.Errorf("read the validation Job %s: %w", record.Name, err)
		}

		switch jobOutcome(&job) {
		case jobFailed:
			return false, fatalf(
				"the target's paths could not be established on node %s, so the cutover would "+
					"strand it; the migration is being taken back", record.Node)
		case jobSucceeded:
			record.Succeeded = true
			passed = true
		default:
			pending++
		}
	}

	// The passes are persisted before they are acted on: a restart must not
	// re-run a check on a node that already passed.
	if passed {
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			status.Migration.ValidationJobs = jobs
		}); err != nil {
			return false, err
		}
	}
	if pending > 0 {
		return false, nil
	}

	// Right before the point of no return, ask once more which nodes consume
	// the subsystem. A node that appeared while the checks ran gets its own,
	// and the step finishes on the pass after that.
	late, err := r.consumingNodes(ctx, ops, subject)
	if err != nil {
		// Blocking a migration that is otherwise ready on a transient listing
		// failure trades a certain delay for an uncertain gain.
		return true, nil //nolint:nilerr // the validated set is what there is
	}
	started, err := r.startValidationJobs(ctx, ops, subject, late)
	if err != nil {
		return false, err
	}
	return started == 0, nil
}

// jobOutcome reads a Job's terminal condition, which the Job controller sets.
type outcome int

const (
	jobRunning outcome = iota
	jobSucceeded
	jobFailed
)

func jobOutcome(job *batchv1.Job) outcome {
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return jobSucceeded
		case batchv1.JobFailed:
			return jobFailed
		}
	}
	return jobRunning
}

// forgetValidationJob drops one node's record so the next pass starts it over.
func (r *PersistentVolumeOpsReconciler) forgetValidationJob(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, name string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		kept := make([]simplyblockv1alpha2.ValidationJob, 0, len(status.Migration.ValidationJobs))
		for _, job := range status.Migration.ValidationJobs {
			if job.Name != name {
				kept = append(kept, job)
			}
		}
		status.Migration.ValidationJobs = kept
	})
}

// deleteValidationJobs removes the Jobs the checks ran in. They have served
// their purpose by the time this is called, and leaving them would mean the
// cleanup that follows raced pods still connecting paths.
func (r *PersistentVolumeOpsReconciler) deleteValidationJobs(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) error {
	if ops.Status.Migration == nil {
		return nil
	}
	for _, record := range ops.Status.Migration.ValidationJobs {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Namespace: record.Namespace, Name: record.Name},
		}
		err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete the validation Job %s: %w", record.Name, err)
		}
	}
	return nil
}

// releaseOnEveryValidatedNode starts the release on every node that took part,
// for a migration that is being given up before the cutover.
//
// It closes the gap a per-node release cannot. The Job that fails releases its
// own paths on the way out, and the nodes whose checks passed exited
// successfully and are never told the migration was abandoned — by another
// node's failure, or by the operator giving up. Their target paths stay
// connected, retry a target that has stopped answering, and settle into the
// husk that blocks the subsystem's next migration.
//
// Best effort by design, and not waited on: the operation's outcome is already
// decided and must not become "still failing" because a cleanup Job is pending.
// What escapes is cleared by the reap the next validation runs first.
func (r *PersistentVolumeOpsReconciler) releaseOnEveryValidatedNode(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) error {
	migration := ops.Status.Migration
	if migration == nil || len(migration.ValidationJobs) == 0 || len(migration.Connections) == 0 {
		// Nothing was checked, so no host connected a target path on this
		// operation's account.
		return nil
	}

	namespace, clusterUUID := migration.ValidationJobs[0].Namespace, migration.ClusterUUID
	if subject != nil {
		namespace = subject.namespace()
	}
	image, err := vmigration.JobImage(ctx, r.Client, namespace, clusterUUID)
	if err != nil {
		return err
	}

	var nodes []string
	for _, record := range migration.ValidationJobs {
		job := r.releaseJob(ops, namespace, record.Node, image)
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("release the target paths on node %s: %w", record.Node, err)
		}
		nodes = append(nodes, record.Node)
	}

	r.event(ops, corev1.EventTypeNormal, ReasonReleasingPaths,
		"Releasing the target paths of subsystem %s on %s",
		migration.SubsystemNQN, strings.Join(nodes, ", "))
	return nil
}

// reapOnEveryValidatedNode clears the husks a migration leaves on the hosts
// that took part, after the cutover, and reports whether every node is done.
//
// It is the release Job's mode, which reaps as well as releases, and running it
// here is safe for the reason that mode is: a release declines to touch a path
// that is serving, and after the cutover the target paths are the ones serving.
// What it does take down is a controller carrying no namespace at all, which is
// the state a path lost mid-check settles into and which blocks the subsystem's
// next migration until something clears it.
//
// Unlike the release on the failure path, this one is waited for. A cleanup
// that cannot finish has to be visible, and the step's deadline is what makes
// it so.
func (r *PersistentVolumeOpsReconciler) reapOnEveryValidatedNode(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (bool, error) {
	migration := ops.Status.Migration
	image, err := vmigration.JobImage(ctx, r.Client, subject.namespace(), migration.ClusterUUID)
	if err != nil {
		return false, err
	}

	done := true
	for _, record := range migration.ValidationJobs {
		job := r.releaseJob(ops, subject.namespace(), record.Node, image)

		var existing batchv1.Job
		err := r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, &existing)
		switch {
		case apierrors.IsNotFound(err):
			if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
				return false, fmt.Errorf("clear the paths on node %s: %w", record.Node, err)
			}
			done = false
		case err != nil:
			return false, fmt.Errorf("read the cleanup Job on node %s: %w", record.Node, err)
		default:
			switch jobOutcome(&existing) {
			case jobFailed:
				r.event(ops, corev1.EventTypeWarning, ReasonCleanupBlocked,
					"The paths of subsystem %s on node %s could not be cleared",
					migration.SubsystemNQN, record.Node)
				return false, fmt.Errorf("the paths on node %s could not be cleared", record.Node)
			case jobRunning:
				done = false
			}
		}
	}
	return done, nil
}

// pathJob builds one node-pinned Job running the given mode against that host's
// NVMe fabric.
//
// It carries no owner reference. A cluster-scoped object cannot own a
// namespaced one — the garbage collector treats such a reference as
// unresolvable and deletes the dependent — so these Jobs are deleted by the
// operation itself, on every path that ends it.
func (r *PersistentVolumeOpsReconciler) pathJob(
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	subject *subject,
	node, image, mode string,
) *batchv1.Job {
	return r.modeJob(ops, subject.namespace(), node, image, mode, 0)
}

// releaseJob is the cleanup counterpart, retried where the check is not:
// nothing downstream waits on a check that failed, so a transient failure there
// that is not retried is simply a path left connected, which is the leak.
func (r *PersistentVolumeOpsReconciler) releaseJob(
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	namespace, node, image string,
) *batchv1.Job {
	return r.modeJob(ops, namespace, node, image, modeRelease, 2)
}

func (r *PersistentVolumeOpsReconciler) modeJob(
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	namespace, node, image, mode string,
	backoffLimit int32,
) *batchv1.Job {
	migration := ops.Status.Migration
	connections, _ := json.Marshal(hostConnections(migration.Connections))

	return vmigration.BuildJob(vmigration.JobParams{
		Name:          jobName(mode, ops.Name, node),
		Namespace:     namespace,
		Hostname:      node,
		Image:         image,
		ContainerName: containerFor(mode),
		Mode:          mode,
		Env: []corev1.EnvVar{
			{Name: "VMIG_CONNECTIONS", Value: string(connections)},
			// Which subsystem this node is expected to be connected to.
			{Name: "VMIG_SUBSYSTEM_NQN", Value: migration.SubsystemNQN},
			// The host's sysfs, mounted into the Job: the container's own /sys
			// is not the host's.
			{Name: "VMIG_SYS_ROOT", Value: "/host/sys"},
		},
		BackoffLimit: backoffLimit,
		TTL:          jobTTL,
		Deadline:     validationJobDeadline,
	})
}

func containerFor(mode string) string {
	if mode == modeRelease {
		return "nvme-release"
	}
	return "nvme-validate"
}

// hostConnections renders the recorded paths into what the Job's binary reads.
func hostConnections(
	conns []simplyblockv1alpha2.MigrationConnection,
) []vmigration.Connection {
	out := make([]vmigration.Connection, 0, len(conns))
	for _, conn := range conns {
		out = append(out, vmigration.Connection{
			NQN:            conn.NQN,
			IP:             conn.Address,
			Port:           int(deref(conn.Port)),
			Transport:      conn.Transport,
			NrIoQueues:     int(deref(conn.NrIOQueues)),
			ReconnectDelay: int(deref(conn.ReconnectDelaySeconds)),
			CtrlLossTmo:    int(deref(conn.CtrlLossTimeoutSeconds)),
			FastIOFailTmo:  int(deref(conn.FastIOFailTimeoutSeconds)),
			KeepAliveTmo:   int(deref(conn.KeepAliveTimeoutSeconds)),
		})
	}
	return out
}

func deref(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}

// jobName is stable for one (mode, operation, node), which is what makes
// creating it idempotent: a pass that created the Job and crashed before
// recording it finds its own Job rather than making a second.
func jobName(mode, ops, node string) string {
	prefix := "pvops-validate-"
	if mode == modeRelease {
		prefix = "pvops-release-"
	}
	return prefix + labelSafe(ops) + "-" + nodeSuffix(node)
}

// nodeSuffix is a DNS-label-safe, collision-resistant suffix for a node name.
// Node names can be long fully qualified names and are not label-safe, so the
// short host part is kept for readability and a hash of the full name for
// uniqueness.
func nodeSuffix(node string) string {
	sum := sha256.Sum256([]byte(node))
	short := labelSafe(strings.SplitN(node, ".", 2)[0])
	if len(short) > 16 {
		short = short[:16]
	}
	if short == "" {
		return hex.EncodeToString(sum[:6])
	}
	return short + "-" + hex.EncodeToString(sum[:4])
}

// nonLabelChars matches everything not allowed inside a DNS-1123 label.
var nonLabelChars = regexp.MustCompile(`[^a-z0-9-]`)

func labelSafe(s string) string {
	s = nonLabelChars.ReplaceAllString(strings.ToLower(s), "")
	if len(s) > 20 {
		s = s[:20]
	}
	return s
}
