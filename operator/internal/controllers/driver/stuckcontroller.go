// Recycling a controller pod that a StatefulSet rollout cannot get past.
//
// A StatefulSet replaces its pods in order and waits for each to be ready, so a
// pod that is unhealthy on a stale revision holds the rollout there: the revision
// that would fix it is never applied to it. Deleting the pod is the only way out.

package driver

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// stuckControllerGrace is how long the controller pod may be unready on a
	// stale revision before it is deleted. A pod that is merely slow to start
	// gets this long, and a plugin that crash-loops does not need longer.
	stuckControllerGrace = 2 * time.Minute

	reasonControllerPodRecycled = "ControllerPodRecycled"
)

// recycleStuckController deletes the controller pod when it has been unready on
// a stale revision for longer than stuckControllerGrace. It returns how long to
// wait before looking again, or zero when there is nothing to wait for.
func (r *SimplyblockDriverReconciler) recycleStuckController(
	ctx context.Context, d *simplyblockv1alpha2.SimplyblockDriver,
) (time.Duration, error) {
	var sts appsv1.StatefulSet
	key := client.ObjectKey{Namespace: d.Namespace, Name: names(d).controllerStatefulSet}
	if err := r.Get(ctx, key, &sts); err != nil {
		return 0, client.IgnoreNotFound(err)
	}

	// Without a rollout in progress there is no newer revision to move a pod to.
	update := sts.Status.UpdateRevision
	if update == "" || update == sts.Status.CurrentRevision || sts.Spec.Selector == nil {
		return 0, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
	if err != nil {
		return 0, fmt.Errorf("read the controller selector: %w", err)
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(d.Namespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return 0, err
	}

	var wait time.Duration
	for i := range pods.Items {
		pod := &pods.Items[i]
		// A pod already on the update revision would come back identical.
		if pod.DeletionTimestamp != nil || podIsReady(pod) ||
			pod.Labels[appsv1.ControllerRevisionHashLabelKey] == update {
			continue
		}
		remaining := stuckControllerGrace - time.Since(unreadySince(pod))
		if remaining > 0 {
			if wait == 0 || remaining < wait {
				wait = remaining
			}
			continue
		}
		if err := r.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
			return 0, err
		}
		r.event(d, corev1.EventTypeWarning, reasonControllerPodRecycled, fmt.Sprintf(
			"controller pod %s was unready on revision %s for over %s, so the rollout to %s could not "+
				"move past it; it was deleted and the StatefulSet recreates it at the update revision",
			pod.Name, pod.Labels[appsv1.ControllerRevisionHashLabelKey], stuckControllerGrace, update))
	}
	return wait, nil
}

func podIsReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// unreadySince is when the pod last stopped being ready, or when it was created
// if it never reported a condition.
func unreadySince(pod *corev1.Pod) time.Time {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time
		}
	}
	return pod.CreationTimestamp.Time
}
