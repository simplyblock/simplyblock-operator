// Scaling the metadata server's StatefulSet to the active plus its standbys,
// and noticing when no standby is running (design-pnfs-mds-standby.md §4.1 and
// §10).
//
// A StatefulSet scale-down removes the highest ordinals, and the active may be
// on one of them. The takeover owns who is active and how it moves, so this
// file asks it through Switchover and scales only once the active is on an
// ordinal that stays.

package mdsstate

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
)

// Switchover is what the scale step needs from the takeover.
type Switchover interface {
	// ActiveOrdinal is the ordinal of the active pod, and false when none has
	// been promoted yet.
	ActiveOrdinal(ctx context.Context, sts *appsv1.StatefulSet) (ordinal int32, known bool, err error)
	// RequestSwitchover asks the takeover to move the active onto an ordinal
	// below keepBelow. It returns once the request is made, not once the
	// switchover is done, and the scale step looks again on its next pass.
	RequestSwitchover(ctx context.Context, sts *appsv1.StatefulSet, keepBelow int32) error
}

// ScaleResult is what a Scale pass did.
type ScaleResult int

const (
	// ScaleDone says the StatefulSet has the wanted replicas.
	ScaleDone ScaleResult = iota + 1
	// ScaleWaitingForSwitchover says the active is on an ordinal that the
	// scale-down would remove, and a switchover was requested.
	ScaleWaitingForSwitchover
)

// Scaler sets the StatefulSet's replicas.
type Scaler struct {
	Client     client.Client
	Switchover Switchover
}

// Scale sets the StatefulSet to the wanted replicas. A scale-up is written at
// once, and the new pods boot as standbys. A scale-down first has the active
// moved off the ordinals it removes. Without a hook, or when the active cannot
// be read, it refuses, since the pod to remove may be the active.
func (s *Scaler) Scale(ctx context.Context, sts *appsv1.StatefulSet, replicas int32) (ScaleResult, error) {
	current := int32(1)
	if sts.Spec.Replicas != nil {
		current = *sts.Spec.Replicas
	}
	if current == replicas {
		return ScaleDone, nil
	}
	if replicas < current {
		if s.Switchover == nil {
			return 0, errors.New("scaling the metadata server down needs to know which pod is active, and no switchover hook is wired")
		}
		active, known, err := s.Switchover.ActiveOrdinal(ctx, sts)
		if err != nil {
			return 0, fmt.Errorf("reading the active metadata server before a scale-down: %w", err)
		}
		if known && active >= replicas {
			if err := s.Switchover.RequestSwitchover(ctx, sts, replicas); err != nil {
				return 0, fmt.Errorf("requesting a switchover below ordinal %d: %w", replicas, err)
			}
			return ScaleWaitingForSwitchover, nil
		}
	}
	patch := client.MergeFrom(sts.DeepCopy())
	sts.Spec.Replicas = ptr.To(replicas)
	if err := s.Client.Patch(ctx, sts, patch); err != nil {
		return 0, fmt.Errorf("scaling %s to %d: %w", sts.Name, replicas, err)
	}
	return ScaleDone, nil
}

// StandbyMissing reports whether standbys is above zero and no pod other than
// the active is running and ready. A pod that is terminating is not a standby.
// Before the first promotion there is no active, and one ready pod is the one
// about to become it.
func StandbyMissing(standbys int32, active string, pods []corev1.Pod) bool {
	if standbys <= 0 {
		return false
	}
	ready := 0
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || p.Name == active || !podReady(p) {
			continue
		}
		ready++
	}
	if active == "" && ready > 0 {
		ready--
	}
	return ready == 0
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
