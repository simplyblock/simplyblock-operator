// The step that makes the database's secondary indices usable, and the Job it
// waits for.
//
// The control plane answers every lookup that is not by primary key from a
// declared secondary index, and an index is only trusted by readers once it has
// been walked and declared ready. Until then those reads fall back to a full
// scan of the table, which is correct and gets slower with every record. The
// walk is the control plane's own code and lives in its image, so the operator
// reaches it the way an administrator would, by running `sbctl cluster
// build-indices`.
//
// It sits immediately after the database becomes available, and that position is
// the whole point. The index keyspace belongs to the deployment rather than to a
// cluster, so nothing has to exist yet for the backfill to run, and a database
// with no records in it is the one moment where every declared index is complete
// the instant it is declared ready. Later is worse in two different ways: after
// the management API is serving, the window between the first write and the
// backfill is served from scans, and after a cluster exists the backfill has a
// table to walk.
//
// The work is a Job rather than an init container on the management API. An init
// container runs once per pod, so a rolling update would run the backfill again
// while the previous image is still serving writes, and an index declared ready
// while a writer that does not maintain it is live is missing those writes for
// good.
//
// The Job names nothing the install has not created yet, which is what the step's
// position costs it. The account, the shared ConfigMap, and the management API's
// serving certificate are all applied two steps later, and a pod that names one
// of them does not fail: no pod is created for a missing account, a missing
// ConfigMap key holds the container in CreateContainerConfigError, and a missing
// Secret holds it in ContainerCreating. So the backfill carries its log level
// literally and claims no account, because `sbctl cluster build-indices` speaks
// to FoundationDB and to nothing in Kubernetes, and the material it presents is
// the database's own peer certificate rather than the management API's serving
// one: FoundationDB's TLS is mutual, so a client reaching coordinators that
// advertise a TLS listener presents a certificate or does not connect, and that
// certificate is issued by the step that created the database.
//
// A Job that has used up its attempts fails the installation. The indices are
// complete the moment they are declared ready on a database this install just
// created, so a Job that cannot get there is the image or the database rather
// than the data, and running it again on an interval reports the same failure
// every fifteen seconds. The install stops on the step, says so on the object
// and in an event, and moves again when something it can act on changes: the
// spec naming another image replaces the Job, and deleting the Job by hand
// creates it again.
//
// Nothing here runs on an upgrade. The installation machine is entered once, so
// an index declared by a later release reaches an existing deployment through
// `sbctl cluster build-indices` after the rollout, never from this step.

package controlplane

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// indexJobAttempts is how many times the backfill runs before Kubernetes
// reports the Job failed and the installation fails on it. The command is
// idempotent and a first attempt that dies on a database still settling is the
// case worth retrying, so a small budget covers the transient half without
// hiding a real refusal for long.
const indexJobAttempts = 3

// indexJobDeadline is the wall clock the whole Job gets, and it is what makes a
// backfill that never starts fail rather than sit active forever: attempts are
// counted from pods that ran and failed, so a pod that is never created and a
// pod that never leaves ContainerCreating count against nothing and leave the
// Job carrying neither condition. It sits inside the step's own budget, because
// a step that expires first reports only that the install is late, while the
// Job's expiry is a Failed condition that says the backfill is the thing that
// did not happen.
const indexJobDeadline = buildingIndicesDeadline - 2*time.Minute

// indexJob is the backfill itself: the control plane's image, the cluster file,
// and the database's peer certificate where the database asks for one.
func indexJob(cp *simplyblockv1alpha2.ControlPlane) *batchv1.Job {
	managed := cp.Spec.Source.Local
	labels := map[string]string{appLabel: indexJobName}

	//nolint:prealloc // the literal is the declaration; the append below is the shared set
	env := []corev1.EnvVar{
		{Name: "LVOL_NVMF_PORT_START", Value: lvolNVMfPortStart},
		namespaceEnv(),
		{Name: "SIMPLYBLOCK_LOG_LEVEL", Value: defaultLogLevel},
	}
	env = append(env, prometheusEnv()...)

	var peerMount []corev1.VolumeMount
	var peerVolume []corev1.Volume
	if fdbPeerTLS(cp) {
		env = append(env, fdbClientEnv()...)
		peerMount = fdbClientMount()
		peerVolume = fdbClientVolume()
	}

	spec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Containers: []corev1.Container{{
			Name:            "build-indices",
			Image:           localImage(cp),
			ImagePullPolicy: pullPolicyOf(managed),
			Command:         []string{"sbctl", "cluster", "build-indices"},
			Env:             env,
			VolumeMounts:    append([]corev1.VolumeMount{clusterFileMount()}, peerMount...),
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		}},
		Volumes: append([]corev1.Volume{clusterFileVolumeSource()}, peerVolume...),
	}
	scheduling(managed, &spec)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      indexJobName,
			Namespace: cp.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			// BackoffLimit counts the restarts after the first run.
			BackoffLimit:          ptr.To(int32(indexJobAttempts - 1)),
			ActiveDeadlineSeconds: ptr.To(int64(indexJobDeadline.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{"log-collector/enabled": "true"},
				},
				Spec: spec,
			},
		},
	}
}

// buildIndices creates the Job on the pass that finds none, and reports the step
// finished on the pass that finds it complete.
//
// It reads before it writes rather than applying like every other step, and a
// Job is the reason: its pod template is immutable, so an apply carrying a
// different image over a Job an earlier install left behind fails on every pass
// instead of running the backfill. Reading first also makes the step idempotent
// in the way the machine expects, since a Job that is already there is a
// backfill that is already running.
//
// A failed Job fails the installation, as an installFailure the machine turns
// into the event and the message. The one exception is a failed Job running an
// image the spec no longer names: the administrator changed the thing that
// failed, so the Job is removed here and created again on the next pass.
func (r *ControlPlaneReconciler) buildIndices(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) (done bool, held string, err error) {
	var job batchv1.Job
	key := client.ObjectKey{Namespace: cp.Namespace, Name: indexJobName}
	switch err := r.Get(ctx, key, &job); {
	case errors.IsNotFound(err):
		if err := applyOne(ctx, r.Client, cp, r.Scheme, indexJob(cp)); err != nil {
			return false, "", err
		}
		return false, fmt.Sprintf("%s has been created and has not run yet", indexJobName), nil
	case err != nil:
		return false, "", fmt.Errorf("read Job %s: %w", indexJobName, err)
	}

	switch jobOutcome(&job) {
	case jobSucceeded:
		return true, "", nil
	case jobFailed:
		if image := jobImage(&job); image != localImage(cp) {
			if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				return false, "", fmt.Errorf("remove the failed Job %s: %w", indexJobName, err)
			}
			return false, fmt.Sprintf("%s failed under %s and is being replaced with one running %s",
				indexJobName, image, localImage(cp)), nil
		}
		return false, "", &installFailure{message: indexFailureMessage(&job)}
	default:
		return false, fmt.Sprintf("%s is still building the database's secondary indices",
			indexJobName), nil
	}
}

// indexFailureMessage is what the installation stops on. It names the failure
// Kubernetes reported, because the two ways this Job fails ask different things
// of whoever reads the message: a Job that used up its attempts ran and refused,
// and its pods say why, while a Job that ran out of wall clock may have produced
// no pod at all, and sending its reader to logs that do not exist wastes the
// reader.
func indexFailureMessage(job *batchv1.Job) string {
	what := fmt.Sprintf("%s failed", indexJobName)
	where := "Fix what the Job's pods report"

	switch jobFailureReason(job) {
	case batchv1.JobReasonBackoffLimitExceeded:
		what = fmt.Sprintf("%s failed %d times", indexJobName, indexJobAttempts)
	case batchv1.JobReasonDeadlineExceeded:
		what = fmt.Sprintf("%s did not finish within %s", indexJobName, indexJobDeadline)
		where = "Look first at whether the Job produced a pod at all, since one that was " +
			"never created or never started is as likely here as a backfill that ran"
	}

	return fmt.Sprintf("%s; the database's secondary indices are not usable, so the "+
		"installation has stopped. %s, then name another image in spec.source.local.image "+
		"or delete the Job to run it again", what, where)
}

// jobImage is the image the backfill container runs, which is what a spec
// change is compared against. A Job somebody built by hand with no container
// compares as no image, and is replaced.
func jobImage(job *batchv1.Job) string {
	containers := job.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return ""
	}
	return containers[0].Image
}

// installFailure is a step reporting that the installation cannot finish from
// where it is. The machine emits it and stops requeueing rather than treating
// it as a transient error to back off on or a hold to poll, because neither a
// backoff nor a poll changes what the step will find.
type installFailure struct {
	message string
}

func (f *installFailure) Error() string {
	return f.message
}

// outcome is what a Job's conditions say about it, which is the only reading of
// one that does not have to interpret counts.
type outcome int

const (
	jobRunning outcome = iota
	jobSucceeded
	jobFailed
)

// jobFailureReason is what Kubernetes called the failure, and the empty string
// for a Job that has not failed.
func jobFailureReason(job *batchv1.Job) string {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return condition.Reason
		}
	}
	return ""
}

// jobOutcome reads those conditions. A Job carries neither condition while it is
// running, including before its first pod is scheduled.
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
