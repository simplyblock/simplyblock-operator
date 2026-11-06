// A contingency, not a steady-state mechanism: the wedge webAPIProbe's split
// between /_meta/health and /_meta/ready was built to expose, where the
// management API's process is up but its calls into FoundationDB never
// return. This has been observed in practice, should not happen, and is worth
// investigating every time it does; what follows is the operator refusing to
// leave such a pod silently serving nothing while nobody notices.

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// DefaultWebAPIWatchdogInterval is how often every management API pod's
	// Ready condition is checked.
	DefaultWebAPIWatchdogInterval = 30 * time.Second

	// DefaultWebAPIUnreadyThreshold is how long a pod that was previously
	// ready must stay unready before the watchdog restarts it. It is well
	// past the readiness probe's own tolerance for a FoundationDB hiccup (six
	// failures five seconds apart, so thirty seconds), so a pod reaching this
	// is not that; it is the wedge this file exists for.
	DefaultWebAPIUnreadyThreshold = 3 * time.Minute

	// webAPIWasReadyAnnotation records, on the pod itself, that it has
	// answered ready at least once. Recording it there rather than in the
	// watchdog's memory is what lets a regression be told apart from a pod
	// still starting even across an operator restart.
	webAPIWasReadyAnnotation = "storage.simplyblock.io/webapi-was-ready"

	// webAPIWasReadyValue is the only value webAPIWasReadyAnnotation is ever
	// set to. Its absence, not a "false," means a pod has not answered ready
	// yet.
	webAPIWasReadyValue = "true"

	// WebAPIPodWedgedReason is the event reason the watchdog raises when it
	// restarts a pod.
	WebAPIPodWedgedReason = "WebAPIPodWedged"
)

// errWebAPIPodWedged is logged rather than returned: detecting the wedge is
// not a failure of this pass, and nothing here failed to run.
var errWebAPIPodWedged = errors.New(
	"a management API pod answered its liveness probe but not its readiness probe long enough to be restarted")

// WebAPIReadinessWatchdog is the contingency itself. Kubernetes alone leaves
// a pod stuck in exactly the state described above running forever: a
// readiness failure only drops a pod from the Service, and nothing restarts
// it. Every occurrence belongs in this operator's error log and in an event
// precisely because it should not be happening and needs a human to look at
// why, but the watchdog restarts the pod anyway rather than wait for one.
//
// It is a Runnable rather than a reconciler for the device health monitor's
// reason (storagedevice_health_monitor.go): nothing about one pod's Ready
// condition changing is what should trigger this, so it runs on its own
// schedule instead.
type WebAPIReadinessWatchdog struct {
	client.Client
	Recorder events.EventRecorder

	// Interval is how often every management API pod is checked.
	Interval time.Duration
	// UnreadyThreshold is how long a pod that was ready must stay unready
	// before the watchdog restarts it.
	UnreadyThreshold time.Duration
	// The clock the watchdog reads, which a test replaces.
	Now func() time.Time
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch;delete

// NeedLeaderElection implements manager.LeaderElectionRunnable: two replicas
// restarting the same wedged pod would race each other for nothing.
func (w *WebAPIReadinessWatchdog) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (w *WebAPIReadinessWatchdog) Start(ctx context.Context) error {
	w.defaults()
	log := logf.FromContext(ctx).WithName("webapi-readiness-watchdog")
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		if err := w.Pass(ctx); err != nil {
			log.Error(err, "checking management API pods for the readiness wedge")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *WebAPIReadinessWatchdog) defaults() {
	if w.Interval == 0 {
		w.Interval = DefaultWebAPIWatchdogInterval
	}
	if w.UnreadyThreshold == 0 {
		w.UnreadyThreshold = DefaultWebAPIUnreadyThreshold
	}
	if w.Now == nil {
		w.Now = time.Now
	}
}

// Pass runs one check of every management API pod. It is exported for the
// test that drives the watchdog pass by pass.
func (w *WebAPIReadinessWatchdog) Pass(ctx context.Context) error {
	w.defaults()
	log := logf.FromContext(ctx).WithName("webapi-readiness-watchdog")

	var pods corev1.PodList
	if err := w.List(ctx, &pods, client.MatchingLabels{appLabel: ComponentWebAPI}); err != nil {
		return fmt.Errorf("list management API pods: %w", err)
	}

	now := w.Now()
	var failures []error
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue // already going; nothing left for this pass to do
		}
		cond := readyCondition(pod)
		if cond == nil {
			continue // the kubelet has not reported a Ready condition yet
		}

		switch cond.Status {
		case corev1.ConditionTrue:
			if err := w.markEverReady(ctx, pod); err != nil {
				failures = append(failures, err)
			}
		case corev1.ConditionFalse:
			if pod.Annotations[webAPIWasReadyAnnotation] != webAPIWasReadyValue {
				// Still on its first readiness pass: starting up, or waiting
				// on a dependency. Reporting that is the readiness probe's
				// job, not this watchdog's.
				continue
			}
			if unreadyFor := now.Sub(cond.LastTransitionTime.Time); unreadyFor >= w.UnreadyThreshold {
				if err := w.restart(ctx, log, pod, unreadyFor); err != nil {
					failures = append(failures, err)
				}
			}
		}
	}
	return errors.Join(failures...)
}

// readyCondition is the pod's own Ready condition, which the kubelet writes
// from the readiness probe's verdict and timestamps on every transition.
func readyCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodReady {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// markEverReady records, on the pod itself, that it has answered ready.
func (w *WebAPIReadinessWatchdog) markEverReady(ctx context.Context, pod *corev1.Pod) error {
	if pod.Annotations[webAPIWasReadyAnnotation] == webAPIWasReadyValue {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[webAPIWasReadyAnnotation] = webAPIWasReadyValue
	if err := w.Patch(ctx, pod, patch); err != nil {
		return fmt.Errorf("mark %s as having answered ready: %w", pod.Name, err)
	}
	return nil
}

// restart is the contingency firing: the pod answered its liveness probe
// throughout, so the process is up, but its readiness probe has failed for
// longer than a FoundationDB hiccup explains. This should not happen, and
// every occurrence is worth investigating on its own; the operator restarts
// the pod anyway rather than leave a replica serving nothing indefinitely
// while nobody notices. The other replicas keep serving in the meantime.
func (w *WebAPIReadinessWatchdog) restart(
	ctx context.Context, log logr.Logger, pod *corev1.Pod, unreadyFor time.Duration,
) error {
	log.Error(errWebAPIPodWedged, "restarting a management API pod",
		"pod", pod.Name, "namespace", pod.Namespace, "unreadyFor", unreadyFor.Round(time.Second).String())
	w.Recorder.Eventf(pod, nil, corev1.EventTypeWarning, WebAPIPodWedgedReason, WebAPIPodWedgedReason,
		"the pod answered its liveness probe throughout but failed its readiness probe for %s; restarting it. "+
			"This should not happen and is worth investigating; the other replicas keep serving in the meantime.",
		unreadyFor.Round(time.Second))
	if err := w.Delete(ctx, pod); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}
