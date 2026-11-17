// Package replication holds the controllers of one-shot operations on an
// application's replication objects.
//
// volumegroupreplicationops_controller.go reconciles a VolumeGroupReplicationOps
// (design-volumegroupreplicationops.md): the Fence action resolves the
// application's VolumeGroupReplication to its consistency group on this site's
// control plane and orders the fence, for a site whose application was failed
// over to another site. The layer that ordered the failover creates the object;
// nothing in the operator decides that a site was superseded.
package replication

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

var (
	// VolumeGroupReplicationKind is the csi-addons kind an operation names.
	VolumeGroupReplicationKind = schema.GroupVersionKind{
		Group: "replication.storage.openshift.io", Version: "v1alpha1", Kind: "VolumeGroupReplication"}
	// VolumeGroupReplicationContentKind carries the driver's group handle.
	VolumeGroupReplicationContentKind = schema.GroupVersionKind{
		Group: "replication.storage.openshift.io", Version: "v1alpha1", Kind: "VolumeGroupReplicationContent"}
)

const (
	// FencingTimeout is how long the Fencing step waits for the control plane to
	// accept the fence before the operation fails.
	FencingTimeout = 30 * time.Minute
	// fenceRetryInterval paces the next attempt after a retryable answer. A site
	// that just reconnected may need minutes before its storage nodes answer.
	fenceRetryInterval = 15 * time.Second

	eventGroupFenced   = "GroupFenced"
	eventFenceRetrying = "FenceRetrying"
	eventFenceFailed   = "FenceFailed"
)

// GroupFencer is the part of the control-plane client the operation uses.
type GroupFencer interface {
	FenceGroup(ctx context.Context, gh lvol.GroupHandle) error
	ConsistencyGroupMembers(ctx context.Context, gh lvol.GroupHandle) ([]string, error)
}

// VolumeGroupReplicationOpsReconciler drives a VolumeGroupReplicationOps to
// completion.
type VolumeGroupReplicationOpsReconciler struct {
	client.Client
	// APIReader reads the csi-addons objects uncached, so no informer is started
	// for kinds this site may not serve.
	APIReader client.Reader
	Recorder  events.EventRecorder
	// ControlPlane builds the client of this site's control plane for one pass.
	ControlPlane func(context.Context) (GroupFencer, error)
	// Now is the clock the step deadline is measured on. Nil is time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumegroupreplicationops,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumegroupreplicationops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=replication.storage.openshift.io,resources=volumegroupreplications;volumegroupreplicationcontents,verbs=get

func (r *VolumeGroupReplicationOpsReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile performs at most one step per pass, so every position the operation
// can be interrupted in is readable from status.
func (r *VolumeGroupReplicationOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ops := &simplyblockv1alpha2.VolumeGroupReplicationOps{}
	if err := r.Get(ctx, req.NamespacedName, ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	switch ops.Status.Phase {
	case simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseSucceeded,
		simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseFailed:
		return ctrl.Result{}, nil
	}
	if ops.Spec.Action != simplyblockv1alpha2.VolumeGroupReplicationOpsActionFence {
		// Admission's enum rejects it. An older CRD may not carry the enum.
		return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf("unknown action %q", ops.Spec.Action))
	}

	switch ops.Status.Step.State {
	case "", simplyblockv1alpha2.VolumeGroupReplicationOpsStepValidating:
		return ctrl.Result{}, r.validate(ctx, ops)
	case simplyblockv1alpha2.VolumeGroupReplicationOpsStepFencing:
		return r.fence(ctx, ops)
	case simplyblockv1alpha2.VolumeGroupReplicationOpsStepReporting:
		return ctrl.Result{}, r.report(ctx, ops)
	}
	return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf("unrecognized step %q", ops.Status.Step.State))
}

// validate resolves the group handle and records it, so later steps act on the
// copy even after Ramen deletes the VolumeGroupReplication.
func (r *VolumeGroupReplicationOpsReconciler) validate(
	ctx context.Context, ops *simplyblockv1alpha2.VolumeGroupReplicationOps,
) error {
	gh, reason := r.resolve(ctx, ops)
	if reason != "" {
		return r.fail(ctx, ops, reason)
	}
	now := r.now()
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.VolumeGroupReplicationOpsStatus) {
		status.Phase = simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseRunning
		status.GroupHandle = gh.String()
		status.Step = simplyblockv1alpha2.VolumeGroupReplicationOpsStepSnapshot{
			State:    simplyblockv1alpha2.VolumeGroupReplicationOpsStepFencing,
			Deadline: &metav1.Time{Time: now.Add(FencingTimeout)},
		}
		status.Message = "ordering the fence of " + gh.String()
		if status.StartedAt == nil {
			status.StartedAt = &metav1.Time{Time: now}
		}
	})
}

// resolve reads the VolumeGroupReplication and its content object. A non-empty
// reason is why the operation cannot proceed.
func (r *VolumeGroupReplicationOpsReconciler) resolve(
	ctx context.Context, ops *simplyblockv1alpha2.VolumeGroupReplicationOps,
) (lvol.GroupHandle, string) {
	vgr := &unstructured.Unstructured{}
	vgr.SetGroupVersionKind(VolumeGroupReplicationKind)
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: ops.Namespace, Name: ops.Spec.VolumeGroupReplicationRef}, vgr)
	if reason := readFailure(err, VolumeGroupReplicationKind, ops.Spec.VolumeGroupReplicationRef); reason != "" {
		return lvol.GroupHandle{}, reason
	}
	contentName, _, _ := unstructured.NestedString(vgr.Object, "spec", "volumeGroupReplicationContentName")
	if contentName == "" {
		return lvol.GroupHandle{}, fmt.Sprintf("VolumeGroupReplication %q names no content object yet",
			ops.Spec.VolumeGroupReplicationRef)
	}
	content := &unstructured.Unstructured{}
	content.SetGroupVersionKind(VolumeGroupReplicationContentKind)
	err = r.APIReader.Get(ctx, types.NamespacedName{Name: contentName}, content)
	if reason := readFailure(err, VolumeGroupReplicationContentKind, contentName); reason != "" {
		return lvol.GroupHandle{}, reason
	}
	handle, _, _ := unstructured.NestedString(content.Object, "spec", "volumeGroupReplicationHandle")
	gh, ok := lvol.ParseGroupHandle(lvol.VolumeHandle(handle))
	if !ok {
		return lvol.GroupHandle{}, fmt.Sprintf(
			"the handle %q of VolumeGroupReplicationContent %q is not a consistency-group handle (cg:<cluster>:<group>)",
			handle, contentName)
	}
	return gh, ""
}

// readFailure turns a failed read into the reason the operation fails, or ""
// when the read succeeded.
func readFailure(err error, gvk schema.GroupVersionKind, name string) string {
	switch {
	case err == nil:
		return ""
	case meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err):
		return fmt.Sprintf("%s (%s) is not served on this cluster", gvk.Kind, gvk.GroupVersion())
	case kerrors.IsNotFound(err):
		return fmt.Sprintf("%s %q not found", gvk.Kind, name)
	default:
		return fmt.Sprintf("reading %s %q: %v", gvk.Kind, name, err)
	}
}

// fence orders the fence until the control plane accepts it, refuses it for
// good, or the step's deadline passes.
func (r *VolumeGroupReplicationOpsReconciler) fence(
	ctx context.Context, ops *simplyblockv1alpha2.VolumeGroupReplicationOps,
) (ctrl.Result, error) {
	if deadline := ops.Status.Step.Deadline; deadline != nil && r.now().After(deadline.Time) {
		return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf(
			"the control plane did not accept the fence within %s: %s", FencingTimeout, ops.Status.Message))
	}
	gh, ok := lvol.ParseGroupHandle(lvol.VolumeHandle(ops.Status.GroupHandle))
	if !ok {
		return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf("recorded group handle %q is malformed", ops.Status.GroupHandle))
	}
	cp, err := r.ControlPlane(ctx)
	if err == nil {
		err = cp.FenceGroup(ctx, gh)
	}
	if err == nil {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, eventGroupFenced, eventGroupFenced,
			"consistency group %s fenced, its replication stopped", gh)
		return ctrl.Result{}, r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.VolumeGroupReplicationOpsStatus) {
			status.Step = simplyblockv1alpha2.VolumeGroupReplicationOpsStepSnapshot{
				State: simplyblockv1alpha2.VolumeGroupReplicationOpsStepReporting,
			}
			status.Message = "fenced " + gh.String()
		})
	}
	var se *controlplane.StatusError
	if errors.As(err, &se) && se.StatusCode >= 400 && se.StatusCode < 500 && se.StatusCode != http.StatusConflict {
		return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf("the control plane refused the fence of %s: %v", gh, err))
	}
	message := fmt.Sprintf("the fence of %s is not accepted yet: %v", gh, err)
	if ops.Status.Message != message {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, eventFenceRetrying, eventFenceRetrying, "%s", message)
	}
	if werr := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.VolumeGroupReplicationOpsStatus) {
		status.Message = message
	}); werr != nil {
		return ctrl.Result{}, werr
	}
	return ctrl.Result{RequeueAfter: fenceRetryInterval}, nil
}

// report records the group's members. The fence already happened, so a member
// list that cannot be read is reported in the message rather than as a failure.
func (r *VolumeGroupReplicationOpsReconciler) report(
	ctx context.Context, ops *simplyblockv1alpha2.VolumeGroupReplicationOps,
) error {
	gh, _ := lvol.ParseGroupHandle(lvol.VolumeHandle(ops.Status.GroupHandle))
	message := "fenced " + gh.String()
	cp, err := r.ControlPlane(ctx)
	var ids []string
	if err == nil {
		ids, err = cp.ConsistencyGroupMembers(ctx, gh)
	}
	if err != nil {
		message = fmt.Sprintf("fenced %s; its members could not be read: %v", gh, err)
		ids = nil
	}
	members := make([]simplyblockv1alpha2.FencedMemberStatus, 0, len(ids))
	for _, id := range ids {
		members = append(members, simplyblockv1alpha2.FencedMemberStatus{VolumeID: id, Fenced: true})
	}
	now := r.now()
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.VolumeGroupReplicationOpsStatus) {
		status.Phase = simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseSucceeded
		status.Step = simplyblockv1alpha2.VolumeGroupReplicationOpsStepSnapshot{
			State: simplyblockv1alpha2.VolumeGroupReplicationOpsStepCompleted,
		}
		status.Members = members
		status.Message = message
		status.CompletedAt = &metav1.Time{Time: now}
	})
}

// fail turns the operation terminally Failed with the reason.
func (r *VolumeGroupReplicationOpsReconciler) fail(
	ctx context.Context, ops *simplyblockv1alpha2.VolumeGroupReplicationOps, message string,
) error {
	r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, eventFenceFailed, eventFenceFailed, "%s", message)
	now := r.now()
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.VolumeGroupReplicationOpsStatus) {
		status.Phase = simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseFailed
		status.Message = message
		status.CompletedAt = &metav1.Time{Time: now}
		if status.StartedAt == nil {
			status.StartedAt = &metav1.Time{Time: now}
		}
	})
}

// writeStatus applies the mutation to a fresh copy under optimistic
// concurrency and stamps observedGeneration on every write.
func (r *VolumeGroupReplicationOpsReconciler) writeStatus(
	ctx context.Context,
	ops *simplyblockv1alpha2.VolumeGroupReplicationOps,
	mutate func(*simplyblockv1alpha2.VolumeGroupReplicationOpsStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &simplyblockv1alpha2.VolumeGroupReplicationOps{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(ops), fresh); err != nil {
			return err
		}
		mutate(&fresh.Status)
		fresh.Status.ObservedGeneration = fresh.Generation
		if err := r.Status().Update(ctx, fresh); err != nil {
			return err
		}
		ops.Status = fresh.Status
		return nil
	})
}

// SetupWithManager registers the reconciler on its own kind only.
func (r *VolumeGroupReplicationOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.VolumeGroupReplicationOps{}).
		Complete(r)
}
