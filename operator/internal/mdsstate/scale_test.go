// Scaling the StatefulSet to 1 + standbys, and noticing when no standby is
// running (design-pnfs-mds-standby.md §4.1 and §10, O-28, O-29, O-41).

package mdsstate

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"
)

// fakeSwitchover stands in for the takeover's hook.
type fakeSwitchover struct {
	active    int32
	known     bool
	activeErr error
	requested []int32
}

func (f *fakeSwitchover) ActiveOrdinal(context.Context, *appsv1.StatefulSet) (int32, bool, error) {
	return f.active, f.known, f.activeErr
}

func (f *fakeSwitchover) RequestSwitchover(_ context.Context, _ *appsv1.StatefulSet, keepBelow int32) error {
	f.requested = append(f.requested, keepBelow)
	return nil
}

func stsWithReplicas(n int32) *appsv1.StatefulSet {
	s := testSTS(nil)
	s.Spec.Replicas = ptr.To(n)
	return s
}

func replicasOf(t *testing.T, s *Scaler, sts *appsv1.StatefulSet) int32 {
	t.Helper()
	return *reload(t, s.Client, sts).Spec.Replicas
}

// O-29: raising the standbys scales up and promotes nothing.
func TestScaleRaisesTheReplicasWithoutASwitchover(t *testing.T) {
	sts := stsWithReplicas(1)
	sw := &fakeSwitchover{known: true, active: 0}
	s := &Scaler{Client: kubeFor(t, sts), Switchover: sw}

	got, err := s.Scale(context.Background(), sts, 2)
	if err != nil || got != ScaleDone {
		t.Fatalf("Scale = %v, %v, want ScaleDone", got, err)
	}
	if n := replicasOf(t, s, sts); n != 2 {
		t.Errorf("replicas = %d, want 2", n)
	}
	if len(sw.requested) != 0 {
		t.Errorf("switchover requested: %v", sw.requested)
	}
}

// O-28 and O-41: the active is on a removed ordinal, so the switchover comes
// first and the scale-down waits for it.
func TestScaleDownSwitchesOverFromARemovedOrdinalFirst(t *testing.T) {
	for _, tc := range []struct {
		name           string
		replicas, want int32
		active         int32
		wantKeepBelow  int32
	}{
		{"1 standby to 0, active on 1", 2, 1, 1, 1},
		{"2 standbys to 1, active on 2", 3, 2, 2, 2},
		{"2 standbys to 0, active on 1", 3, 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sts := stsWithReplicas(tc.replicas)
			sw := &fakeSwitchover{known: true, active: tc.active}
			s := &Scaler{Client: kubeFor(t, sts), Switchover: sw}

			got, err := s.Scale(context.Background(), sts, tc.want)
			if err != nil || got != ScaleWaitingForSwitchover {
				t.Fatalf("Scale = %v, %v, want ScaleWaitingForSwitchover", got, err)
			}
			if n := replicasOf(t, s, sts); n != tc.replicas {
				t.Errorf("replicas = %d, want %d left as they were", n, tc.replicas)
			}
			if len(sw.requested) != 1 || sw.requested[0] != tc.wantKeepBelow {
				t.Errorf("switchover requests = %v, want one with keepBelow %d", sw.requested, tc.wantKeepBelow)
			}

			// The takeover moved the active below the cut, and the next pass scales.
			sw.active = 0
			got, err = s.Scale(context.Background(), reload(t, s.Client, sts), tc.want)
			if err != nil || got != ScaleDone {
				t.Fatalf("second Scale = %v, %v, want ScaleDone", got, err)
			}
			if n := replicasOf(t, s, sts); n != tc.want {
				t.Errorf("replicas = %d, want %d", n, tc.want)
			}
		})
	}
}

func TestScaleDownWithTheActiveOnAKeptOrdinalNeedsNoSwitchover(t *testing.T) {
	sts := stsWithReplicas(3)
	sw := &fakeSwitchover{known: true, active: 1}
	s := &Scaler{Client: kubeFor(t, sts), Switchover: sw}
	if got, err := s.Scale(context.Background(), sts, 2); err != nil || got != ScaleDone {
		t.Fatalf("Scale = %v, %v", got, err)
	}
	if len(sw.requested) != 0 || replicasOf(t, s, sts) != 2 {
		t.Errorf("requests %v, replicas %d", sw.requested, replicasOf(t, s, sts))
	}
}

// Before the first promotion there is no active to move.
func TestScaleDownBeforeAnyActiveNeedsNoSwitchover(t *testing.T) {
	sts := stsWithReplicas(2)
	s := &Scaler{Client: kubeFor(t, sts), Switchover: &fakeSwitchover{known: false}}
	if got, err := s.Scale(context.Background(), sts, 1); err != nil || got != ScaleDone {
		t.Fatalf("Scale = %v, %v", got, err)
	}
}

// Not knowing who is active is not a license to remove a pod that may be it.
func TestScaleDownRefusesWithoutAHookOrWhenTheActiveIsUnreadable(t *testing.T) {
	sts := stsWithReplicas(2)
	s := &Scaler{Client: kubeFor(t, sts)}
	if got, err := s.Scale(context.Background(), sts, 1); err == nil || got == ScaleDone {
		t.Errorf("Scale without a hook = %v, %v, want a refusal", got, err)
	}
	s = &Scaler{Client: kubeFor(t, sts), Switchover: &fakeSwitchover{activeErr: errors.New("boom")}}
	if got, err := s.Scale(context.Background(), sts, 1); err == nil || got == ScaleDone {
		t.Errorf("Scale with an unreadable active = %v, %v, want a refusal", got, err)
	}
	if n := replicasOf(t, s, sts); n != 2 {
		t.Errorf("replicas = %d, want 2", n)
	}
}

func TestScaleAtTheWantedCountWritesNothing(t *testing.T) {
	sts := stsWithReplicas(2)
	c := kubeFor(t, sts)
	before := reload(t, c, sts).ResourceVersion
	s := &Scaler{Client: c, Switchover: &fakeSwitchover{known: true}}
	if got, err := s.Scale(context.Background(), sts, 2); err != nil || got != ScaleDone {
		t.Fatalf("Scale = %v, %v", got, err)
	}
	if after := reload(t, c, sts).ResourceVersion; after != before {
		t.Error("the StatefulSet was written with nothing to change")
	}
}

func pod(name string, ready, deleting bool) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	if deleting {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	}
	return p
}

func TestStandbyMissing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		standbys int32
		active   string
		pods     []corev1.Pod
		want     bool
	}{
		{"none wanted", 0, "mds-0", []corev1.Pod{pod("mds-0", true, false)}, false},
		{"a ready standby", 1, "mds-0", []corev1.Pod{pod("mds-0", true, false), pod("mds-1", true, false)}, false},
		{"the standby is pending", 1, "mds-0", []corev1.Pod{pod("mds-0", true, false)}, true},
		{"the standby is not ready", 1, "mds-0", []corev1.Pod{pod("mds-0", true, false), pod("mds-1", false, false)}, true},
		{"the standby is terminating", 1, "mds-0", []corev1.Pod{pod("mds-0", true, false), pod("mds-1", true, true)}, true},
		{"only the active is ready", 2, "mds-1", []corev1.Pod{pod("mds-0", false, false), pod("mds-1", true, false)}, true},
		{"one of two standbys ready", 2, "mds-1", []corev1.Pod{pod("mds-0", true, false), pod("mds-1", true, false), pod("mds-2", false, false)}, false},
		{"no active yet, two ready pods", 1, "", []corev1.Pod{pod("mds-0", true, false), pod("mds-1", true, false)}, false},
		{"no active yet, one ready pod", 1, "", []corev1.Pod{pod("mds-0", true, false)}, true},
		{"no pods", 1, "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StandbyMissing(tc.standbys, tc.active, tc.pods); got != tc.want {
				t.Errorf("StandbyMissing = %v, want %v", got, tc.want)
			}
		})
	}
}
