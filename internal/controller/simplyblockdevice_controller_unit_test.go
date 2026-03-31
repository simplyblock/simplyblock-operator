package controller

import (
	"context"
	"strings"
	"testing"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDeviceActionCompleted(t *testing.T) {
	r := &SimplyBlockDeviceReconciler{}

	tests := []struct {
		name   string
		action string
		status string
		want   bool
	}{
		{
			name:   "remove success",
			action: utils.DeviceActionRemove,
			status: utils.DeviceStatusRemoved,
			want:   true,
		},
		{
			name:   "remove not yet",
			action: utils.DeviceActionRemove,
			status: "online",
			want:   false,
		},
		{
			name:   "restart success",
			action: utils.DeviceActionRestart,
			status: utils.DeviceStatusOnline,
			want:   true,
		},
		{
			name:   "restart not yet",
			action: utils.DeviceActionRestart,
			status: "restarting",
			want:   false,
		},
		{
			name:   "unknown action",
			action: "noop",
			status: "anything",
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.deviceActionCompleted(tc.action, tc.status)
			if got != tc.want {
				t.Fatalf("deviceActionCompleted(%q,%q) = %v, want %v", tc.action, tc.status, got, tc.want)
			}
		})
	}
}

func TestReconcileDeviceActionTransitions(t *testing.T) {
	t.Run("initializes running status when action status is missing", func(t *testing.T) {
		dev := &simplyblockv1alpha1.SimplyBlockDevice{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "dev-a",
				Namespace:  "default",
				Generation: 7,
			},
			Spec: simplyblockv1alpha1.SimplyBlockDeviceSpec{
				Action:   utils.DeviceActionRemove,
				DeviceID: "d1",
				NodeUUID: "n1",
			},
		}

		r := newDeviceStateTestReconciler(t, dev)
		res, err := r.reconcileDeviceAction(context.Background(), dev, "cluster", "secret")
		if err != nil {
			t.Fatalf("reconcileDeviceAction returned error: %v", err)
		}
		if !res.Requeue {
			t.Fatalf("expected requeue when initializing action status")
		}
		if dev.Status.ActionStatus == nil {
			t.Fatalf("expected action status to be initialized")
		}
		if dev.Status.ActionStatus.Action != utils.DeviceActionRemove {
			t.Fatalf("unexpected action initialized: %q", dev.Status.ActionStatus.Action)
		}
		if dev.Status.ActionStatus.State != utils.ActionStateRunning {
			t.Fatalf("unexpected state initialized: %q", dev.Status.ActionStatus.State)
		}
		if dev.Status.ActionStatus.ObservedGeneration != dev.Generation {
			t.Fatalf("expected observedGeneration=%d, got %d", dev.Generation, dev.Status.ActionStatus.ObservedGeneration)
		}
	})

	t.Run("does not re-enter terminal success for same generation", func(t *testing.T) {
		dev := &simplyblockv1alpha1.SimplyBlockDevice{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "dev-b",
				Namespace:  "default",
				Generation: 5,
			},
			Spec: simplyblockv1alpha1.SimplyBlockDeviceSpec{
				Action:   utils.DeviceActionRestart,
				DeviceID: "d1",
				NodeUUID: "n1",
			},
			Status: simplyblockv1alpha1.SimplyBlockDeviceStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action:             utils.DeviceActionRestart,
					State:              utils.ActionStateSuccess,
					ObservedGeneration: 5,
					Triggered:          true,
				},
			},
		}

		r := newDeviceStateTestReconciler(t, dev)
		res, err := r.reconcileDeviceAction(context.Background(), dev, "cluster", "secret")
		if err != nil {
			t.Fatalf("reconcileDeviceAction returned error: %v", err)
		}
		if res.Requeue || res.RequeueAfter != 0 {
			t.Fatalf("expected no requeue for terminal success, got %+v", res)
		}
		if !dev.Status.ActionStatus.Triggered {
			t.Fatalf("expected action status to remain terminal and unchanged")
		}
	})

	t.Run("re-initializes running status when action changes", func(t *testing.T) {
		dev := &simplyblockv1alpha1.SimplyBlockDevice{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "dev-c",
				Namespace:  "default",
				Generation: 3,
			},
			Spec: simplyblockv1alpha1.SimplyBlockDeviceSpec{
				Action:   utils.DeviceActionRemove,
				DeviceID: "d1",
				NodeUUID: "n1",
			},
			Status: simplyblockv1alpha1.SimplyBlockDeviceStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action:             utils.DeviceActionRestart,
					State:              utils.ActionStateSuccess,
					ObservedGeneration: 2,
				},
			},
		}

		r := newDeviceStateTestReconciler(t, dev)
		res, err := r.reconcileDeviceAction(context.Background(), dev, "cluster", "secret")
		if err != nil {
			t.Fatalf("reconcileDeviceAction returned error: %v", err)
		}
		if !res.Requeue {
			t.Fatalf("expected requeue after action change initialization")
		}
		if dev.Status.ActionStatus.Action != utils.DeviceActionRemove {
			t.Fatalf("expected action to be reset to remove, got %q", dev.Status.ActionStatus.Action)
		}
		if dev.Status.ActionStatus.State != utils.ActionStateRunning {
			t.Fatalf("expected state running after action change, got %q", dev.Status.ActionStatus.State)
		}
	})

	t.Run("rejects unsupported action transition", func(t *testing.T) {
		dev := &simplyblockv1alpha1.SimplyBlockDevice{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "dev-d",
				Namespace:  "default",
				Generation: 2,
			},
			Spec: simplyblockv1alpha1.SimplyBlockDeviceSpec{
				Action:   "invalid-action",
				DeviceID: "d1",
				NodeUUID: "n1",
			},
			Status: simplyblockv1alpha1.SimplyBlockDeviceStatus{
				ActionStatus: &simplyblockv1alpha1.ActionStatus{
					Action: "invalid-action",
					State:  utils.ActionStateRunning,
				},
			},
		}

		r := newDeviceStateTestReconciler(t, dev)
		_, err := r.reconcileDeviceAction(context.Background(), dev, "cluster", "secret")
		if err == nil {
			t.Fatalf("expected error for unsupported device action")
		}
		if !strings.Contains(err.Error(), "unsupported device action") {
			t.Fatalf("unexpected error: %v", err)
		}
		if dev.Status.ActionStatus.State != utils.ActionStateRunning {
			t.Fatalf("unsupported action should not transition to success, got %q", dev.Status.ActionStatus.State)
		}
	})
}

func TestFailDeviceActionTransitionsToFailed(t *testing.T) {
	dev := &simplyblockv1alpha1.SimplyBlockDevice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dev-f",
			Namespace: "default",
		},
		Status: simplyblockv1alpha1.SimplyBlockDeviceStatus{
			ActionStatus: &simplyblockv1alpha1.ActionStatus{
				Action: utils.DeviceActionRemove,
				State:  utils.ActionStateRunning,
			},
		},
	}

	r := newDeviceStateTestReconciler(t, dev)
	_, err := r.failDeviceAction(context.Background(), dev, context.DeadlineExceeded)
	if err != nil {
		t.Fatalf("failDeviceAction returned error: %v", err)
	}
	if dev.Status.ActionStatus.State != utils.ActionStateFailed {
		t.Fatalf("expected failed state, got %q", dev.Status.ActionStatus.State)
	}
	if !strings.Contains(dev.Status.ActionStatus.Message, "deadline") {
		t.Fatalf("expected failure message to be set, got %q", dev.Status.ActionStatus.Message)
	}
}

func TestReconcileDeviceActionRejectsIllegalSuccessState(t *testing.T) {
	dev := &simplyblockv1alpha1.SimplyBlockDevice{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "dev-illegal",
			Namespace:  "default",
			Generation: 10,
		},
		Spec: simplyblockv1alpha1.SimplyBlockDeviceSpec{
			Action:   utils.DeviceActionRemove,
			DeviceID: "d1",
			NodeUUID: "n1",
		},
		Status: simplyblockv1alpha1.SimplyBlockDeviceStatus{
			// Illegal/stale success: observedGeneration does not match current generation.
			ActionStatus: &simplyblockv1alpha1.ActionStatus{
				Action:             utils.DeviceActionRemove,
				State:              utils.ActionStateSuccess,
				ObservedGeneration: 9,
				Triggered:          true,
			},
		},
	}

	r := newDeviceStateTestReconciler(t, dev)
	res, err := r.reconcileDeviceAction(context.Background(), dev, "cluster", "secret")
	if err != nil {
		t.Fatalf("reconcileDeviceAction returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue after rejecting illegal success state, got %+v", res)
	}
	if dev.Status.ActionStatus.State != utils.ActionStateFailed {
		t.Fatalf("expected illegal success to be rejected and moved to failed, got %q", dev.Status.ActionStatus.State)
	}
}

func newDeviceStateTestReconciler(t *testing.T, objects ...client.Object) *SimplyBlockDeviceReconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add API scheme: %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha1.SimplyBlockDevice{}).
		WithObjects(objects...).
		Build()

	return &SimplyBlockDeviceReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
