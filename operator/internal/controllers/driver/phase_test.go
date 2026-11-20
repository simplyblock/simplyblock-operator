// U-11 to U-19: the phase and the counts it is explained by.
//
// U-14 against U-15 is the pair design §4.2 rests on. Every node plugin down
// still provisions, and a controller plugin down does not, so the two must not
// collapse into one phase.

package driver

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func ds(ready, desired int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{Status: appsv1.DaemonSetStatus{
		NumberReady:            ready,
		DesiredNumberScheduled: desired,
	}}
}

func sts(ready int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{ReadyReplicas: ready}}
}

func TestPhase(t *testing.T) {
	const (
		installing  = simplyblockv1alpha2.SimplyblockDriverPhaseInstalling
		ready       = simplyblockv1alpha2.SimplyblockDriverPhaseReady
		degraded    = simplyblockv1alpha2.SimplyblockDriverPhaseDegraded
		unavailable = simplyblockv1alpha2.SimplyblockDriverPhaseUnavailable
	)

	tests := []struct {
		name       string
		node       *appsv1.DaemonSet
		controller *appsv1.StatefulSet
		registered bool
		want       simplyblockv1alpha2.SimplyblockDriverPhase
	}{
		{
			name: "the workloads are not applied yet",
			want: installing,
		},
		{
			// U-12
			name:       "every plugin ready and the registration in place",
			node:       ds(3, 3),
			controller: sts(1),
			registered: true,
			want:       ready,
		},
		{
			name:       "every plugin ready but the registration is missing",
			node:       ds(3, 3),
			controller: sts(1),
			registered: false,
			want:       installing,
		},
		{
			// U-13: a partial failure, and every other worker still attaches.
			name:       "one node plugin of three not ready",
			node:       ds(2, 3),
			controller: sts(1),
			registered: true,
			want:       degraded,
		},
		{
			// U-14: no worker can attach, and provisioning still works. This is
			// the row that must not read Unavailable.
			name:       "no node plugin ready while the controller serves",
			node:       ds(0, 3),
			controller: sts(1),
			registered: true,
			want:       degraded,
		},
		{
			// U-15: provisioning has stopped, whatever the node plugins do.
			name:       "the controller plugin is not running",
			node:       ds(3, 3),
			controller: sts(0),
			registered: true,
			want:       unavailable,
		},
		{
			// U-18: the controller decides it even when the node count agrees.
			name:       "the controller is down and every node plugin is ready",
			node:       ds(3, 3),
			controller: sts(0),
			registered: true,
			want:       unavailable,
		},
		{
			name:       "both plugins down",
			node:       ds(0, 3),
			controller: sts(0),
			registered: true,
			want:       unavailable,
		},
		{
			// U-17: a selector matching no worker is a configuration somebody
			// wrote, not a fault, so it is reported rather than failed.
			name:       "the selector matches no worker",
			node:       ds(0, 0),
			controller: sts(1),
			registered: true,
			want:       ready,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := derive(tc.node, tc.controller, tc.registered)
			if got.phase != tc.want {
				t.Errorf("phase = %q, want %q (message: %s)", got.phase, tc.want, got.message)
			}
			if got.message == "" {
				t.Error("the phase carries no message saying why it is what it is")
			}
		})
	}
}

// U-11 and U-16: the counts come off the DaemonSet, and zero ready is a number
// rather than an absence.
func TestCounts(t *testing.T) {
	got := derive(ds(0, 4), sts(1), true)

	if got.nodesReady != 0 || got.nodesTotal != 4 {
		t.Errorf("counts = %d/%d, want 0/4", got.nodesReady, got.nodesTotal)
	}
	if !got.controllerReady {
		t.Error("controllerReady is false with a ready replica")
	}

	none := derive(nil, nil, false)
	if none.nodesReady != 0 || none.nodesTotal != 0 || none.controllerReady {
		t.Errorf("an unapplied deployment reports %d/%d controller=%v, want zeroes",
			none.nodesReady, none.nodesTotal, none.controllerReady)
	}
}

// The message names the numbers, because a phase without them sends a reader to
// kubectl describe to learn which worker is short.
func TestMessageExplainsTheCounts(t *testing.T) {
	got := derive(ds(2, 3), sts(1), true)
	for _, want := range []string{"2", "3"} {
		if !contains(got.message, want) {
			t.Errorf("message %q does not name %q", got.message, want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
