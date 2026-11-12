// The watchdog that recreates a pNFS metadata server pod stuck unready. It
// lives with the MDS StatefulSet it recovers.
//
// Kubernetes leaves an unready pod running: readiness only drops it from its
// Service. The MDS runner restarts its own container when a booted guest stays
// unhealthy, but a container restart cannot recover a guest whose state volume
// lost its device on the host, because only a new pod stages the volume again.
// Every pNFS client in the cluster goes through the MDS, so the operator
// deletes the pod and the StatefulSet creates a fresh one.

package driver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// DefaultMDSWatchdogInterval is how often every MDS pod is checked.
	DefaultMDSWatchdogInterval = 30 * time.Second

	// DefaultMDSUnreadyThreshold is how long an MDS pod that was ready may
	// stay unready before it is recreated. It is past the runner's own
	// 90 s deadline, so the container restart gets its chance first.
	DefaultMDSUnreadyThreshold = 3 * time.Minute

	// MDSPodStuckReason is the event reason raised when a pod is recreated.
	MDSPodStuckReason = "MDSPodStuck"

	// mdsWasReadyAnnotation records on the pod that it has been ready once,
	// so a pod that regressed is told apart from one still booting, across
	// operator restarts too.
	mdsWasReadyAnnotation = "storage.simplyblock.io/mds-was-ready"
	mdsWasReadyValue      = "true"
)

var errMDSPodStuck = errors.New("a pNFS metadata server pod stayed unready after it had served")

// MDSReadinessWatchdog recreates an MDS pod that has been unready too long. It
// is a Runnable rather than a reconciler, like the management API's watchdog,
// because the passage of time is what triggers it, not a change to the pod.
type MDSReadinessWatchdog struct {
	client.Client
	Recorder events.EventRecorder

	// Interval is how often every MDS pod is checked.
	Interval time.Duration
	// UnreadyThreshold is how long a pod that was ready may stay unready.
	UnreadyThreshold time.Duration
	// The clock the watchdog reads, which a test replaces.
	Now func() time.Time
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch;delete

// NeedLeaderElection implements manager.LeaderElectionRunnable, so two
// replicas never delete the same pod.
func (w *MDSReadinessWatchdog) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (w *MDSReadinessWatchdog) Start(ctx context.Context) error {
	w.defaults()
	log := logf.FromContext(ctx).WithName("mds-readiness-watchdog")
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		if err := w.Pass(ctx); err != nil {
			log.Error(err, "checking pNFS metadata server pods")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *MDSReadinessWatchdog) defaults() {
	if w.Interval == 0 {
		w.Interval = DefaultMDSWatchdogInterval
	}
	if w.UnreadyThreshold == 0 {
		w.UnreadyThreshold = DefaultMDSUnreadyThreshold
	}
	if w.Now == nil {
		w.Now = time.Now
	}
}

// mdsPodSelector selects every MDS pod by the cluster label only they carry.
func mdsPodSelector() (labels.Selector, error) {
	req, err := labels.NewRequirement(MDSClusterLabel, selection.Exists, nil)
	if err != nil {
		return nil, err
	}
	return labels.NewSelector().Add(*req), nil
}

// Pass runs one check of every MDS pod.
func (w *MDSReadinessWatchdog) Pass(ctx context.Context) error {
	w.defaults()
	log := logf.FromContext(ctx).WithName("mds-readiness-watchdog")

	selector, err := mdsPodSelector()
	if err != nil {
		return err
	}
	var pods corev1.PodList
	if err := w.List(ctx, &pods, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return fmt.Errorf("list pNFS metadata server pods: %w", err)
	}

	now := w.Now()
	var failures []error
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		cond := podReadyCondition(pod)
		if cond == nil {
			continue
		}
		switch cond.Status {
		case corev1.ConditionTrue:
			if err := w.markWasReady(ctx, pod); err != nil {
				failures = append(failures, err)
			}
		case corev1.ConditionFalse:
			if pod.Annotations[mdsWasReadyAnnotation] != mdsWasReadyValue {
				continue // still booting: the runner's boot deadline covers it
			}
			if unready := now.Sub(cond.LastTransitionTime.Time); unready >= w.UnreadyThreshold {
				if err := w.recreate(ctx, log, pod, unready); err != nil {
					failures = append(failures, err)
				}
			}
		}
	}
	return errors.Join(failures...)
}

func podReadyCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodReady {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

func (w *MDSReadinessWatchdog) markWasReady(ctx context.Context, pod *corev1.Pod) error {
	if pod.Annotations[mdsWasReadyAnnotation] == mdsWasReadyValue {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[mdsWasReadyAnnotation] = mdsWasReadyValue
	if err := w.Patch(ctx, pod, patch); err != nil {
		return fmt.Errorf("mark %s as having been ready: %w", pod.Name, err)
	}
	return nil
}

// recreate deletes the pod, preconditioned on its UID so a replacement that
// already exists under the same name is never deleted by a stale view.
func (w *MDSReadinessWatchdog) recreate(
	ctx context.Context, log logr.Logger, pod *corev1.Pod, unready time.Duration,
) error {
	log.Error(errMDSPodStuck, "recreating a pNFS metadata server pod",
		"pod", pod.Name, "namespace", pod.Namespace, "unreadyFor", unready.Round(time.Second).String())
	w.Recorder.Eventf(pod, nil, corev1.EventTypeWarning, MDSPodStuckReason, MDSPodStuckReason,
		"the metadata server was unready for %s after it had served; recreating the pod so its "+
			"state volume is staged again. Every pNFS client stalls until the new pod is ready.",
		unready.Round(time.Second))
	uid := pod.UID
	if err := w.Delete(ctx, pod, client.Preconditions{UID: &uid}); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}
