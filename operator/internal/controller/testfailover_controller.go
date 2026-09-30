// The TestFailover controller drives one non-disruptive test-failover drill to a
// terminal phase and holds it there until the object is deleted.
//
// It coordinates from the hub: it reads the source on its cluster, resolves a
// recovery point on the recovery cluster's backend, clones it there, and places
// the clone as a bound PVC in an isolated namespace on the recovery cluster,
// without ever touching the source. Deleting the object reclaims what the drill
// created. See operator/docs/designs/design-test-failover.md.
//
// This file is built in slices: the state graph and the drill's lifecycle
// scaffolding land first, and each step's side effects (source read, snapshot,
// clone, placement, teardown) fill in behind the graph the reconcile already
// walks.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/statemachine"
	workv1 "open-cluster-management.io/api/work/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// csiDriverName is the CSI driver the bubble's PersistentVolume adopts the clone
// through, on the recovery cluster.
const csiDriverName = "csi.simplyblock.io"

// testFailoverIDLabel tags every object a drill creates, so teardown can
// enumerate and prove nothing was left behind.
const testFailoverIDLabel = "storage.simplyblock.io/test-id"

// managedClusterViewGVK is the OCM read primitive the hub uses to project a
// managed cluster's object back to itself. It is driven unstructured to avoid a
// dependency on the multicloud-operators-foundation module that defines it.
var managedClusterViewGVK = schema.GroupVersionKind{
	Group:   "view.open-cluster-management.io",
	Version: "v1beta1",
	Kind:    "ManagedClusterView",
}

// finalizerTestFailover holds the object until its drill is torn down, so a
// clone, a drill-taken snapshot, or a placed PVC is never orphaned by a delete
// that races the controller.
const finalizerTestFailover = "storage.simplyblock.io/testfailover-teardown"

// testFailoverStepRequeue is how long the reconcile waits before re-entering a
// step that is still in progress. Named so the controller's cadence is tunable
// in one place (reconciler-patterns §7).
const testFailoverStepRequeue = 10 * time.Second

// testFailoverStepTimeout bounds how long any one step may take. A step that
// blows it fails the drill rather than holding forever, and because the deadline
// lives in status it survives an operator restart (reconciler-patterns §3).
const testFailoverStepTimeout = 15 * time.Minute

// testFailoverGraph is the drill's provisioning state graph: the ordered steps
// from reading the source to a placed, bound PVC. Every state arms a per-step
// deadline on entry. Teardown (Releasing) is not in this graph; it runs on the
// deletion path, off the finalizer, not as a forward transition.
func testFailoverGraph() statemachine.Config[simplyblockv1alpha2.TestFailoverStep] {
	armDeadline := func(context.Context, simplyblockv1alpha2.TestFailoverStep, simplyblockv1alpha2.TestFailoverStep) (time.Duration, error) {
		return testFailoverStepTimeout, nil
	}
	return statemachine.Config[simplyblockv1alpha2.TestFailoverStep]{
		Initial: simplyblockv1alpha2.TestFailoverStepResolvingSource,
		States: map[simplyblockv1alpha2.TestFailoverStep]statemachine.StateDef[simplyblockv1alpha2.TestFailoverStep]{
			simplyblockv1alpha2.TestFailoverStepResolvingSource: {
				To:      []simplyblockv1alpha2.TestFailoverStep{simplyblockv1alpha2.TestFailoverStepResolvingPoint},
				OnEnter: armDeadline,
			},
			simplyblockv1alpha2.TestFailoverStepResolvingPoint: {
				To: []simplyblockv1alpha2.TestFailoverStep{
					simplyblockv1alpha2.TestFailoverStepShipping,
					simplyblockv1alpha2.TestFailoverStepCloning,
				},
				OnEnter: armDeadline,
			},
			simplyblockv1alpha2.TestFailoverStepShipping: {
				To:      []simplyblockv1alpha2.TestFailoverStep{simplyblockv1alpha2.TestFailoverStepCloning},
				OnEnter: armDeadline,
			},
			simplyblockv1alpha2.TestFailoverStepCloning: {
				To:      []simplyblockv1alpha2.TestFailoverStep{simplyblockv1alpha2.TestFailoverStepPlacing},
				OnEnter: armDeadline,
			},
			// Placing is terminal in the provisioning graph: once the bubble PVC is
			// bound, the drill's phase is Ready and it holds until deleted.
			simplyblockv1alpha2.TestFailoverStepPlacing: {OnEnter: armDeadline},
		},
	}
}

// TestFailoverReconciler reconciles a TestFailover object.
type TestFailoverReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=testfailovers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=testfailovers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=testfailovers/finalizers,verbs=update
// +kubebuilder:rbac:groups=work.open-cluster-management.io,resources=manifestworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=view.open-cluster-management.io,resources=managedclusterviews,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives one drill: it ensures the finalizer, walks the provisioning
// graph to Ready, holds there, and tears the drill down on deletion.
func (r *TestFailoverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var tf simplyblockv1alpha2.TestFailover
	if err := r.Get(ctx, req.NamespacedName, &tf); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tf.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, &tf)
	}

	// A drill that never got its finalizer gets it before any side effect, so
	// teardown is guaranteed a chance to run.
	if controllerutil.AddFinalizer(&tf, finalizerTestFailover) {
		if err := r.Update(ctx, &tf); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// A terminal phase re-reconciles to nothing. Ready holds until the object is
	// deleted, and Failed stays as the record. The finalizer is retained in both,
	// so teardown still runs on delete.
	if tf.Status.Phase == simplyblockv1alpha2.TestFailoverPhaseReady ||
		tf.Status.Phase == simplyblockv1alpha2.TestFailoverPhaseFailed {
		return ctrl.Result{}, nil
	}

	log.V(1).Info("reconciling test-failover drill", "phase", tf.Status.Phase, "step", tf.Status.Step.State)
	return r.advanceDrill(ctx, &tf)
}

// advanceDrill walks the provisioning graph one step per reconcile. It builds
// the machine from status.step, so the position survives a restart, and never
// runs a step's side effect from construction.
func (r *TestFailoverReconciler) advanceDrill(ctx context.Context, tf *simplyblockv1alpha2.TestFailover) (ctrl.Result, error) {
	machine, err := statemachine.NewFromSnapshot(ctx, testFailoverGraph(),
		statemachine.FromKube[simplyblockv1alpha2.TestFailoverStep](tf.Status.Step))
	if err != nil {
		return r.fail(ctx, tf, "invalid drill step: "+err.Error())
	}
	defer machine.Close()

	// Nobody has reconciled this yet: enter the initial step.
	if tf.Status.Step.State == "" {
		return r.begin(ctx, tf, machine.CurrentState())
	}

	// A step that blew its deadline fails the drill rather than holding forever.
	if machine.TimeoutReached() {
		return r.fail(ctx, tf, "step "+string(machine.CurrentState())+" exceeded its deadline")
	}

	switch machine.CurrentState() {
	case simplyblockv1alpha2.TestFailoverStepResolvingSource:
		return r.resolveSource(ctx, tf)
	case simplyblockv1alpha2.TestFailoverStepResolvingPoint:
		return r.resolvePoint(ctx, tf)
	case simplyblockv1alpha2.TestFailoverStepCloning:
		return r.cloneRecoveryPoint(ctx, tf)
	case simplyblockv1alpha2.TestFailoverStepPlacing:
		return r.placeBubble(ctx, tf)
	default:
		// Later slices implement the remaining steps; until then a step in
		// progress holds rather than blocks, and re-enters on the requeue.
		return r.hold(ctx, tf, "step "+string(machine.CurrentState())+" not yet implemented")
	}
}

// resolveSource reads the source on its cluster through ManagedClusterViews to
// learn the source volume's backend handle, then advances to ResolvingPoint. It
// is non-blocking: each view is created once and its result awaited across
// reconciles, so a restart re-enters rather than re-creates.
func (r *TestFailoverReconciler) resolveSource(ctx context.Context, tf *simplyblockv1alpha2.TestFailover) (ctrl.Result, error) {
	if tf.Spec.Scope == simplyblockv1alpha2.TestFailoverScopeGroup {
		return r.resolveSourceGroup(ctx, tf)
	}

	pvc, ready, err := r.projectedSource(ctx, tf, "src-pvc", "persistentvolumeclaims", tf.Spec.SourceRef, tf.Spec.SourceNamespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return r.hold(ctx, tf, "waiting for the source PVC projection from cluster "+tf.Spec.SourceCluster)
	}
	pvName, _, _ := unstructured.NestedString(pvc, "spec", "volumeName")
	if pvName == "" {
		return r.fail(ctx, tf, "source PVC "+tf.Spec.SourceRef+" is not bound to a volume")
	}

	pv, ready, err := r.projectedSource(ctx, tf, "src-pv", "persistentvolumes", pvName, "")
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return r.hold(ctx, tf, "waiting for the source PV projection from cluster "+tf.Spec.SourceCluster)
	}
	handle, _, _ := unstructured.NestedString(pv, "spec", "csi", "volumeHandle")
	if handle == "" {
		return r.fail(ctx, tf, "source PV "+pvName+" has no CSI volume handle")
	}
	srcAttrs, _, _ := unstructured.NestedStringMap(pv, "spec", "csi", "volumeAttributes")
	bubbleVC := bubbleVolumeContext(srcAttrs)
	fsType, _, _ := unstructured.NestedString(pv, "spec", "csi", "fsType")

	if err := r.transitionTo(ctx, tf, simplyblockv1alpha2.TestFailoverStepResolvingPoint, func(s *simplyblockv1alpha2.TestFailoverStatus) {
		s.Clones = []simplyblockv1alpha2.TestFailoverClone{{
			SourceRef:           tf.Spec.SourceRef,
			SourceHandle:        handle,
			SourceVolumeContext: bubbleVC,
			SourceFSType:        fsType,
		}}
		s.Message = "resolved the source volume; resolving the recovery point"
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(tf, nil, corev1.EventTypeNormal, "SourceResolved", "SourceResolved",
		"resolved source volume %s on cluster %s", handle, tf.Spec.SourceCluster)
	return ctrl.Result{Requeue: true}, nil
}

// resolveSourceGroup resolves a consistency group's members into one clone slot
// each, then advances to ResolvingPoint. The members and their K8s identity come
// from the source cluster's backend (a group member carries only an lvol id);
// the shared class metadata (fsType and volumeAttributes, identical across
// members of one StorageClass) is read once from a representative member's PV
// through a ManagedClusterView. It is non-blocking: the representative views are
// created once and awaited across reconciles.
func (r *TestFailoverReconciler) resolveSourceGroup(ctx context.Context, tf *simplyblockv1alpha2.TestFailover) (ctrl.Result, error) {
	srcUUID, err := r.resolveGroupSourceUUID(ctx, tf)
	if err != nil {
		return r.hold(ctx, tf, "resolving the source cluster's backend UUID: "+err.Error())
	}

	api := webapi.NewClient()
	if secret, secErr := r.clusterSecret(ctx, tf.Namespace, tf.Spec.SourceCluster); secErr == nil && secret != "" {
		ctx = webapi.WithBearerToken(ctx, secret)
	}

	group, err := api.GetConsistencyGroupByName(ctx, srcUUID, tf.Spec.SourceRef)
	if err != nil {
		return ctrl.Result{}, err
	}
	if group == nil {
		return r.fail(ctx, tf, "consistency group "+tf.Spec.SourceRef+" not found on cluster "+tf.Spec.SourceCluster)
	}
	memberIDs, err := api.GetConsistencyGroupMembers(ctx, srcUUID, group.UUID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(memberIDs) == 0 {
		return r.fail(ctx, tf, "consistency group "+tf.Spec.SourceRef+" has no members")
	}
	memberVols, err := api.ResolveMemberVolumes(ctx, srcUUID, memberIDs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(memberVols) != len(memberIDs) {
		return r.hold(ctx, tf, fmt.Sprintf("resolved %d of %d group members' source volumes; retrying", len(memberVols), len(memberIDs)))
	}

	// One member's PV carries the class metadata every member shares, so a single
	// projection serves the whole group.
	rep := memberVols[memberIDs[0]]
	pvc, ready, err := r.projectedSource(ctx, tf, "src-pvc", "persistentvolumeclaims", rep.PVCName, rep.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return r.hold(ctx, tf, "waiting for the source PVC projection for group member "+rep.PVCName)
	}
	pvName, _, _ := unstructured.NestedString(pvc, "spec", "volumeName")
	if pvName == "" {
		return r.fail(ctx, tf, "group member PVC "+rep.PVCName+" is not bound to a volume")
	}
	pv, ready, err := r.projectedSource(ctx, tf, "src-pv", "persistentvolumes", pvName, "")
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return r.hold(ctx, tf, "waiting for the source PV projection for group member "+rep.PVCName)
	}
	srcAttrs, _, _ := unstructured.NestedStringMap(pv, "spec", "csi", "volumeAttributes")
	bubbleVC := bubbleVolumeContext(srcAttrs)
	fsType, _, _ := unstructured.NestedString(pv, "spec", "csi", "fsType")

	clones := make([]simplyblockv1alpha2.TestFailoverClone, 0, len(memberIDs))
	for _, id := range memberIDs {
		v := memberVols[id]
		clones = append(clones, simplyblockv1alpha2.TestFailoverClone{
			SourceRef:           v.PVCName,
			SourceHandle:        srcUUID + ":" + v.PoolID + ":" + v.LvolID,
			SourceFSType:        fsType,
			SourceVolumeContext: bubbleVC,
			SizeBytes:           v.Size,
		})
	}

	if err := r.transitionTo(ctx, tf, simplyblockv1alpha2.TestFailoverStepResolvingPoint, func(s *simplyblockv1alpha2.TestFailoverStatus) {
		s.Clones = clones
		s.Message = fmt.Sprintf("resolved the consistency group's %d members; resolving the recovery point", len(clones))
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(tf, nil, corev1.EventTypeNormal, "SourceResolved", "SourceResolved",
		"resolved consistency group %s (%d members) on cluster %s", tf.Spec.SourceRef, len(clones), tf.Spec.SourceCluster)
	return ctrl.Result{Requeue: true}, nil
}

// resolveGroupSourceUUID returns the backend UUID of the source cluster. It
// prefers the canonical resolution (a raw UUID, or a local StorageCluster named
// like the cluster), and falls back to the sole local StorageCluster when the
// hub is colocated on the source cluster, where the OCM cluster name does not
// match the StorageCluster name.
func (r *TestFailoverReconciler) resolveGroupSourceUUID(ctx context.Context, tf *simplyblockv1alpha2.TestFailover) (string, error) {
	if uuid, err := utils.ResolveClusterUUID(ctx, r.Client, tf.Namespace, tf.Spec.SourceCluster); err == nil && uuid != "" {
		return uuid, nil
	}
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(tf.Namespace)); err != nil {
		return "", err
	}
	uuid, ready := "", 0
	for i := range clusters.Items {
		if clusters.Items[i].Status.UUID != "" {
			uuid = clusters.Items[i].Status.UUID
			ready++
		}
	}
	if ready == 1 {
		return uuid, nil
	}
	return "", fmt.Errorf("no unique backend UUID for source cluster %q (%d local Storage Clusters with a UUID)", tf.Spec.SourceCluster, ready)
}

// projectedSource ensures a ManagedClusterView for one source object exists on
// the source cluster and returns the projected object once the view controller
// has fetched it. ready is false while the projection is still pending, which is
// the reconcile's cue to hold.
func (r *TestFailoverReconciler) projectedSource(ctx context.Context, tf *simplyblockv1alpha2.TestFailover, suffix, resourceKind, name, namespace string) (obj map[string]interface{}, ready bool, err error) {
	viewName := testFailoverViewName(tf, suffix)
	view := &unstructured.Unstructured{}
	view.SetGroupVersionKind(managedClusterViewGVK)
	getErr := r.Get(ctx, client.ObjectKey{Namespace: tf.Spec.SourceCluster, Name: viewName}, view)
	if apierrors.IsNotFound(getErr) {
		if createErr := r.Create(ctx, newManagedClusterView(tf, viewName, resourceKind, name, namespace)); createErr != nil {
			return nil, false, createErr
		}
		return nil, false, nil
	}
	if getErr != nil {
		return nil, false, getErr
	}
	result, found, nestedErr := unstructured.NestedMap(view.Object, "status", "result")
	if nestedErr != nil || !found || len(result) == 0 {
		return nil, false, nil
	}
	return result, true, nil
}

// newManagedClusterView builds a view that asks the source cluster to project one
// object back to the hub. It lives in the source cluster's namespace on the hub
// and is labeled with the drill's test-id for teardown enumeration.
func newManagedClusterView(tf *simplyblockv1alpha2.TestFailover, name, resourceKind, targetName, targetNamespace string) *unstructured.Unstructured {
	scope := map[string]interface{}{"resource": resourceKind, "name": targetName}
	if targetNamespace != "" {
		scope["namespace"] = targetNamespace
	}
	view := &unstructured.Unstructured{}
	view.SetGroupVersionKind(managedClusterViewGVK)
	view.SetNamespace(tf.Spec.SourceCluster)
	view.SetName(name)
	view.SetLabels(map[string]string{testFailoverIDLabel: string(tf.UID)})
	_ = unstructured.SetNestedMap(view.Object, scope, "spec", "scope")
	return view
}

// testFailoverViewName is a deterministic, bounded name for one of a drill's
// views, so a restart finds the existing view instead of creating a second.
func testFailoverViewName(tf *simplyblockv1alpha2.TestFailover, suffix string) string {
	h := sha256.Sum256([]byte(tf.Namespace + "/" + tf.Name))
	return fmt.Sprintf("tfo-%x-%s", h[:6], suffix)
}

// transitionTo validates the edge against the graph and persists the new step
// together with any status mutation.
func (r *TestFailoverReconciler) transitionTo(ctx context.Context, tf *simplyblockv1alpha2.TestFailover, to simplyblockv1alpha2.TestFailoverStep, mutate func(*simplyblockv1alpha2.TestFailoverStatus)) error {
	machine, err := statemachine.NewFromSnapshot(ctx, testFailoverGraph(),
		statemachine.FromKube[simplyblockv1alpha2.TestFailoverStep](tf.Status.Step))
	if err != nil {
		return err
	}
	defer machine.Close()
	if err := machine.TransitionTo(ctx, to); err != nil {
		return err
	}
	snap := statemachine.ToKube(machine.Snapshot())
	return r.patchStatus(ctx, tf, func(s *simplyblockv1alpha2.TestFailoverStatus) {
		s.Step = snap
		if mutate != nil {
			mutate(s)
		}
	})
}

// begin records that the drill has started and enters the initial step. It
// refuses to start a second active drill against the same source and bubble, so
// two drills cannot duplicate each other's snapshots and clones (design §7.3).
func (r *TestFailoverReconciler) begin(ctx context.Context, tf *simplyblockv1alpha2.TestFailover, initial simplyblockv1alpha2.TestFailoverStep) (ctrl.Result, error) {
	if other, err := r.conflictingActiveDrill(ctx, tf); err != nil {
		return ctrl.Result{}, err
	} else if other != "" {
		return r.fail(ctx, tf, "another active drill "+other+" is running for the same source and bubble cluster")
	}

	now := metav1.Now()
	deadline := metav1.NewTime(now.Add(testFailoverStepTimeout))
	if err := r.patchStatus(ctx, tf, func(s *simplyblockv1alpha2.TestFailoverStatus) {
		s.Phase = simplyblockv1alpha2.TestFailoverPhaseProvisioning
		s.Step = statemachine.KubeSnapshot{State: string(initial), Deadline: &deadline}
		s.Message = "resolving the source"
		if s.StartedAt == nil {
			s.StartedAt = &now
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(tf, nil, corev1.EventTypeNormal, "DrillStarted", "DrillStarted", "the test-failover drill started")
	return ctrl.Result{Requeue: true}, nil
}

// conflictingActiveDrill returns the name of another non-terminal drill against
// the same source and bubble, or empty when there is none. A drill that has
// Failed or is tearing down no longer holds resources and does not conflict.
func (r *TestFailoverReconciler) conflictingActiveDrill(ctx context.Context, tf *simplyblockv1alpha2.TestFailover) (string, error) {
	var drills simplyblockv1alpha2.TestFailoverList
	if err := r.List(ctx, &drills, client.InNamespace(tf.Namespace)); err != nil {
		return "", err
	}
	for i := range drills.Items {
		other := &drills.Items[i]
		if other.UID == tf.UID || !other.DeletionTimestamp.IsZero() {
			continue
		}
		if other.Status.Phase == simplyblockv1alpha2.TestFailoverPhaseFailed ||
			other.Status.Phase == simplyblockv1alpha2.TestFailoverPhaseTearingDown {
			continue
		}
		if other.Spec.Scope == tf.Spec.Scope &&
			other.Spec.SourceRef == tf.Spec.SourceRef &&
			other.Spec.BubbleCluster == tf.Spec.BubbleCluster {
			return other.Name, nil
		}
	}
	return "", nil
}

// hold keeps the drill on its current step and re-enters after the requeue.
func (r *TestFailoverReconciler) hold(ctx context.Context, tf *simplyblockv1alpha2.TestFailover, message string) (ctrl.Result, error) {
	if err := r.patchStatus(ctx, tf, func(s *simplyblockv1alpha2.TestFailoverStatus) {
		s.Message = message
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: testFailoverStepRequeue}, nil
}

// fail records a terminal failure. The finalizer is retained, so teardown still
// runs on delete.
func (r *TestFailoverReconciler) fail(ctx context.Context, tf *simplyblockv1alpha2.TestFailover, message string) (ctrl.Result, error) {
	now := metav1.Now()
	if err := r.patchStatus(ctx, tf, func(s *simplyblockv1alpha2.TestFailoverStatus) {
		s.Phase = simplyblockv1alpha2.TestFailoverPhaseFailed
		s.Message = message
		if s.CompletedAt == nil {
			s.CompletedAt = &now
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(tf, nil, corev1.EventTypeWarning, "DrillFailed", "DrillFailed", "%s", message)
	return ctrl.Result{}, nil
}

// reconcileDeletion tears the drill down and removes the finalizer only once
// every drill resource is confirmed gone. It reclaims in dependency order, and
// each step tolerates a not-found (a re-run after a partial teardown is safe). A
// reclaim that cannot be confirmed holds the object in TearingDown rather than
// clearing the finalizer and orphaning backend storage.
func (r *TestFailoverReconciler) reconcileDeletion(ctx context.Context, tf *simplyblockv1alpha2.TestFailover) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(tf, finalizerTestFailover) {
		return ctrl.Result{}, nil
	}
	if tf.Status.Phase != simplyblockv1alpha2.TestFailoverPhaseTearingDown {
		_ = r.patchStatus(ctx, tf, func(s *simplyblockv1alpha2.TestFailoverStatus) {
			s.Phase = simplyblockv1alpha2.TestFailoverPhaseTearingDown
			s.Step = statemachine.KubeSnapshot{State: string(simplyblockv1alpha2.TestFailoverStepReleasing)}
			s.Message = "tearing down the bubble"
		})
	}

	// The ManifestWork's removal garbage-collects the PV and PVC on the recovery
	// cluster, so it goes first.
	if err := r.deleteManifestWork(ctx, tf); err != nil {
		return r.reclaimPending(ctx, tf, "remove the bubble placement", err)
	}

	// Reclaim every clone slot (one for a volume drill, one per member for a
	// group drill). Each reclaim tolerates a not-found, so a re-run after a
	// partial teardown is safe.
	for i := range tf.Status.Clones {
		clone := tf.Status.Clones[i]
		if clone.CloneID != "" {
			if err := r.reclaimClone(ctx, tf, clone.CloneID); err != nil {
				return r.reclaimPending(ctx, tf, "reclaim the clone for "+clone.SourceRef, err)
			}
		}
		// Only a snapshot the drill took is the drill's to delete; a replicated or
		// pinned one is left alone.
		if clone.SnapshotTaken && clone.SnapshotID != "" {
			if err := r.deleteDrillSnapshot(ctx, tf, clone.SnapshotID); err != nil {
				return r.reclaimPending(ctx, tf, "delete the drill snapshot for "+clone.SourceRef, err)
			}
		}
	}

	// The read-side views cost nothing to leave, but teardown proves no test-id
	// object remains, so they go too.
	if err := r.deleteView(ctx, tf, "src-pvc"); err != nil {
		return r.reclaimPending(ctx, tf, "remove the source view", err)
	}
	if err := r.deleteView(ctx, tf, "src-pv"); err != nil {
		return r.reclaimPending(ctx, tf, "remove the source view", err)
	}

	controllerutil.RemoveFinalizer(tf, finalizerTestFailover)
	if err := r.Update(ctx, tf); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reclaimPending records that teardown is holding on a reclaim it could not
// confirm, keeping the finalizer so nothing is orphaned.
func (r *TestFailoverReconciler) reclaimPending(ctx context.Context, tf *simplyblockv1alpha2.TestFailover, what string, cause error) (ctrl.Result, error) {
	_ = r.patchStatus(ctx, tf, func(s *simplyblockv1alpha2.TestFailoverStatus) {
		s.Message = "teardown is holding: could not " + what + ": " + cause.Error()
	})
	r.Recorder.Eventf(tf, nil, corev1.EventTypeWarning, "ReclaimPending", "ReclaimPending",
		"teardown could not %s: %v", what, cause)
	return ctrl.Result{}, cause
}

// deleteManifestWork removes the bubble's ManifestWork, tolerating an already-gone
// or already-deleting one.
func (r *TestFailoverReconciler) deleteManifestWork(ctx context.Context, tf *simplyblockv1alpha2.TestFailover) error {
	var mw workv1.ManifestWork
	err := r.Get(ctx, client.ObjectKey{Namespace: tf.Spec.BubbleCluster, Name: testFailoverManifestWorkName(tf)}, &mw)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !mw.DeletionTimestamp.IsZero() {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, &mw))
}

// deleteView removes one of the drill's ManagedClusterViews, tolerating a
// not-found.
func (r *TestFailoverReconciler) deleteView(ctx context.Context, tf *simplyblockv1alpha2.TestFailover, suffix string) error {
	view := &unstructured.Unstructured{}
	view.SetGroupVersionKind(managedClusterViewGVK)
	view.SetNamespace(tf.Spec.SourceCluster)
	view.SetName(testFailoverViewName(tf, suffix))
	return client.IgnoreNotFound(r.Delete(ctx, view))
}

// reclaimClone deletes the drill's clone from the recovery cluster's backend.
func (r *TestFailoverReconciler) reclaimClone(ctx context.Context, tf *simplyblockv1alpha2.TestFailover, handle string) error {
	cluster, pool, vol, ok := splitHandle(handle)
	if !ok {
		return nil
	}
	apiClient := webapi.NewClient()
	if secret, err := r.clusterSecret(ctx, tf.Namespace, tf.Spec.BubbleCluster); err == nil && secret != "" {
		ctx = webapi.WithBearerToken(ctx, secret)
	}
	return backendDelete(ctx, apiClient, "reclaim clone",
		fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s", cluster, pool, vol))
}

// deleteDrillSnapshot deletes a snapshot the drill took, from the source
// cluster's backend.
func (r *TestFailoverReconciler) deleteDrillSnapshot(ctx context.Context, tf *simplyblockv1alpha2.TestFailover, handle string) error {
	cluster, pool, snap, ok := splitHandle(handle)
	if !ok {
		return nil
	}
	apiClient := webapi.NewClient()
	if secret, err := r.clusterSecret(ctx, tf.Namespace, tf.Spec.SourceCluster); err == nil && secret != "" {
		ctx = webapi.WithBearerToken(ctx, secret)
	}
	return backendDelete(ctx, apiClient, "delete snapshot",
		fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/snapshots/%s", cluster, pool, snap))
}

// backendDelete issues an idempotent DELETE, treating a not-found as success.
func backendDelete(ctx context.Context, api *webapi.Client, what, endpoint string) error {
	body, status, err := api.Do(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if status == http.StatusNotFound || status < 300 {
		return nil
	}
	return fmt.Errorf("%s: status %d: %s", what, status, string(body))
}

// sourceUnchanged re-reads the source PV projection and reports whether the
// source is still the same backend volume the drill recovered from. It is
// best-effort: when the projection cannot be re-read it does not fail the drill,
// but a projection that shows a different handle is a genuine invariant breach.
func (r *TestFailoverReconciler) sourceUnchanged(ctx context.Context, tf *simplyblockv1alpha2.TestFailover) bool {
	if len(tf.Status.Clones) == 0 || tf.Status.Clones[0].SourceHandle == "" {
		return true
	}
	pv, ok := r.readViewResult(ctx, tf, "src-pv")
	if !ok {
		return true
	}
	handle, _, _ := unstructured.NestedString(pv, "spec", "csi", "volumeHandle")
	if handle == "" {
		return true
	}
	return handle == tf.Status.Clones[0].SourceHandle
}

// readViewResult reads an existing view's projected object without creating one.
func (r *TestFailoverReconciler) readViewResult(ctx context.Context, tf *simplyblockv1alpha2.TestFailover, suffix string) (map[string]interface{}, bool) {
	view := &unstructured.Unstructured{}
	view.SetGroupVersionKind(managedClusterViewGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: tf.Spec.SourceCluster, Name: testFailoverViewName(tf, suffix)}, view); err != nil {
		return nil, false
	}
	result, found, err := unstructured.NestedMap(view.Object, "status", "result")
	if err != nil || !found || len(result) == 0 {
		return nil, false
	}
	return result, true
}

// patchStatus applies mutate to the status and writes it, recording
// observedGeneration so a stale status can be told from a current one.
func (r *TestFailoverReconciler) patchStatus(ctx context.Context, tf *simplyblockv1alpha2.TestFailover, mutate func(*simplyblockv1alpha2.TestFailoverStatus)) error {
	base := client.MergeFrom(tf.DeepCopy())
	mutate(&tf.Status)
	tf.Status.ObservedGeneration = tf.Generation
	return r.Status().Patch(ctx, tf, base)
}

// replicatedSnapshotResult is the control plane's ReplicatedSnapshotDTO for the
// latest replicated snapshot on a DR-target backend.
type replicatedSnapshotResult struct {
	SnapshotID string    `json:"snapshot_id"`
	ClusterID  string    `json:"cluster_id"`
	PoolID     string    `json:"pool_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// resolvePoint resolves the recovery point on the recovery cluster's backend and
// advances to Cloning. In-place (the bubble is the source's own cluster) it takes
// a fresh snapshot of the source, or reuses a pinned one; onto a DR target it
// uses the latest replicated snapshot already there. It records the point as a
// CSI snapshot handle so Cloning is self-contained.
func (r *TestFailoverReconciler) resolvePoint(ctx context.Context, tf *simplyblockv1alpha2.TestFailover) (ctrl.Result, error) {
	if tf.Spec.Scope == simplyblockv1alpha2.TestFailoverScopeGroup {
		return r.resolvePointGroup(ctx, tf)
	}
	if len(tf.Status.Clones) == 0 || tf.Status.Clones[0].SourceHandle == "" {
		return r.fail(ctx, tf, "internal: the source was not resolved before ResolvingPoint")
	}
	srcCluster, srcPool, srcLvol, ok := splitHandle(tf.Status.Clones[0].SourceHandle)
	if !ok {
		return r.fail(ctx, tf, "source handle is malformed: "+tf.Status.Clones[0].SourceHandle)
	}

	apiClient := webapi.NewClient()
	if secret, err := r.clusterSecret(ctx, tf.Namespace, tf.Spec.SourceCluster); err == nil && secret != "" {
		ctx = webapi.WithBearerToken(ctx, secret)
	}

	var snapCluster, snapPool, snapUUID string
	var taken bool
	var pointTime *metav1.Time

	switch {
	case tf.Spec.BubbleCluster == tf.Spec.SourceCluster && tf.Spec.RecoveryPoint != "":
		// In-place, pinned: reuse the operator's chosen snapshot, do not delete it.
		snapCluster, snapPool, snapUUID, taken = srcCluster, srcPool, tf.Spec.RecoveryPoint, false
	case tf.Spec.BubbleCluster == tf.Spec.SourceCluster:
		// In-place: take a fresh snapshot of the source, idempotently.
		id, err := r.ensureSnapshot(ctx, apiClient, srcCluster, srcPool, srcLvol, testFailoverSnapshotName(tf))
		if err != nil {
			return ctrl.Result{}, err
		}
		now := metav1.Now()
		snapCluster, snapPool, snapUUID, taken, pointTime = srcCluster, srcPool, id, true, &now
	default:
		// Onto a DR target: the replicated snapshot is already on that backend.
		dto, found, err := r.latestReplicatedSnapshot(ctx, apiClient, srcCluster, srcLvol)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !found {
			return r.fail(ctx, tf, "no replicated snapshot on the target for volume "+srcLvol+" yet")
		}
		snapCluster, snapPool, snapUUID, taken = dto.ClusterID, dto.PoolID, dto.SnapshotID, false
		if !dto.CreatedAt.IsZero() {
			pt := metav1.NewTime(dto.CreatedAt)
			pointTime = &pt
		}
	}
	if snapPool == "" {
		snapPool = srcPool
	}
	pointHandle := snapCluster + ":" + snapPool + ":" + snapUUID

	if err := r.transitionTo(ctx, tf, simplyblockv1alpha2.TestFailoverStepCloning, func(s *simplyblockv1alpha2.TestFailoverStatus) {
		s.Clones[0].SnapshotID = pointHandle
		s.Clones[0].SnapshotTaken = taken
		if s.Report == nil {
			s.Report = &simplyblockv1alpha2.TestFailoverReport{}
		}
		s.Report.BubbleCluster = tf.Spec.BubbleCluster
		s.Report.RecoveryPoint = snapUUID
		s.Report.RecoveryPointTime = pointTime
		s.Message = "resolved the recovery point; cloning it into the bubble"
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(tf, nil, corev1.EventTypeNormal, "RecoveryPointResolved", "RecoveryPointResolved",
		"recovery point %s on cluster %s", snapUUID, snapCluster)
	return ctrl.Result{Requeue: true}, nil
}

// resolvePointGroup resolves the group's one group-consistent recovery point on
// the target and records one snapshot handle per clone slot, then advances to
// Cloning. The point comes from the group's replication policy's latest
// generation, which the control plane returns only when every member has a
// snapshot at the same generation, so the recovered set is crash-consistent.
func (r *TestFailoverReconciler) resolvePointGroup(ctx context.Context, tf *simplyblockv1alpha2.TestFailover) (ctrl.Result, error) {
	if len(tf.Status.Clones) == 0 {
		return r.fail(ctx, tf, "internal: the group source was not resolved before ResolvingPoint")
	}
	srcUUID, err := r.resolveGroupSourceUUID(ctx, tf)
	if err != nil {
		return r.hold(ctx, tf, "resolving the source cluster's backend UUID: "+err.Error())
	}

	api := webapi.NewClient()
	if secret, secErr := r.clusterSecret(ctx, tf.Namespace, tf.Spec.SourceCluster); secErr == nil && secret != "" {
		ctx = webapi.WithBearerToken(ctx, secret)
	}

	group, err := api.GetConsistencyGroupByName(ctx, srcUUID, tf.Spec.SourceRef)
	if err != nil {
		return ctrl.Result{}, err
	}
	if group == nil {
		return r.fail(ctx, tf, "consistency group "+tf.Spec.SourceRef+" not found on cluster "+tf.Spec.SourceCluster)
	}
	policyID, err := api.ResolveGroupPolicyID(ctx, srcUUID, group.LvsName, group.NodeID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if policyID == "" {
		return r.fail(ctx, tf, "no consistency-group replication policy found for group "+tf.Spec.SourceRef)
	}

	groupSeq, members, found, err := api.LatestReplicatedGeneration(ctx, srcUUID, policyID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !found {
		return r.fail(ctx, tf, "no replicated generation on the target for group "+tf.Spec.SourceRef+" yet")
	}
	if len(members) != len(tf.Status.Clones) {
		return r.fail(ctx, tf, fmt.Sprintf("the group generation has %d members but %d were resolved; group membership changed mid-drill", len(members), len(tf.Status.Clones)))
	}

	if err := r.transitionTo(ctx, tf, simplyblockv1alpha2.TestFailoverStepCloning, func(s *simplyblockv1alpha2.TestFailoverStatus) {
		// Every member is at one generation, so any one-to-one assignment of the
		// generation's snapshots to the clone slots yields a crash-consistent set;
		// a precise source-to-target mapping is a later refinement.
		for i := range members {
			s.Clones[i].SnapshotID = members[i].ClusterID + ":" + members[i].PoolID + ":" + members[i].SnapshotID
			s.Clones[i].SnapshotTaken = false
		}
		if s.Report == nil {
			s.Report = &simplyblockv1alpha2.TestFailoverReport{}
		}
		s.Report.BubbleCluster = tf.Spec.BubbleCluster
		s.Report.RecoveryPoint = fmt.Sprintf("generation %d", groupSeq)
		s.Message = fmt.Sprintf("resolved the group-consistent point (generation %d); cloning %d members", groupSeq, len(members))
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(tf, nil, corev1.EventTypeNormal, "RecoveryPointResolved", "RecoveryPointResolved",
		"group-consistent generation %d on cluster %s (%d members)", groupSeq, tf.Spec.BubbleCluster, len(members))
	return ctrl.Result{Requeue: true}, nil
}

// ensureSnapshot returns the id of the source volume's snapshot named name,
// taking it if it does not exist yet. Ask-then-act: it lists the pool's snapshots
// first, so a retry after a crash between create and status-write reuses the
// snapshot rather than taking a second (reconciler-patterns §4).
func (r *TestFailoverReconciler) ensureSnapshot(ctx context.Context, api *webapi.Client, clusterUUID, poolID, lvolUUID, name string) (string, error) {
	listEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/snapshots", clusterUUID, poolID)
	body, status, err := api.Do(ctx, http.MethodGet, listEndpoint, nil)
	if err != nil || status >= 300 {
		return "", requestError("list snapshots", body, status, err)
	}
	var existing []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.Unmarshal(body, &existing)
	for _, s := range existing {
		if s.Name == name {
			return s.ID, nil
		}
	}

	createEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/snapshots", clusterUUID, poolID, lvolUUID)
	_, header, status, err := api.DoWithHeaders(ctx, http.MethodPost, createEndpoint,
		map[string]interface{}{"name": name, "backup": false})
	if err != nil || status >= 300 {
		return "", requestError("take snapshot", nil, status, err)
	}
	id := lastPathSegment(header.Get("Location"))
	if id == "" {
		return "", fmt.Errorf("take snapshot: no snapshot id in the Location header")
	}
	return id, nil
}

// latestReplicatedSnapshot reads the latest replicated snapshot for a source
// volume on its DR target. found is false when replication has landed nothing.
func (r *TestFailoverReconciler) latestReplicatedSnapshot(ctx context.Context, api *webapi.Client, sourceClusterUUID, sourceLvolUUID string) (replicatedSnapshotResult, bool, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/replication/relationships/%s/latest-snapshot", sourceClusterUUID, sourceLvolUUID)
	body, status, err := api.Do(ctx, http.MethodGet, endpoint, nil)
	if status == http.StatusNotFound {
		return replicatedSnapshotResult{}, false, nil
	}
	if err != nil || status >= 300 {
		return replicatedSnapshotResult{}, false, requestError("resolve latest replicated snapshot", body, status, err)
	}
	var dto replicatedSnapshotResult
	if err := json.Unmarshal(body, &dto); err != nil {
		return replicatedSnapshotResult{}, false, fmt.Errorf("decode latest-snapshot: %w", err)
	}
	return dto, dto.SnapshotID != "", nil
}

// cloneRecoveryPoint clones the resolved recovery point into a writable volume on
// the recovery cluster's backend and advances to Placing. The clone is built on
// the same backend the point lives on, so no data crosses a cluster boundary.
func (r *TestFailoverReconciler) cloneRecoveryPoint(ctx context.Context, tf *simplyblockv1alpha2.TestFailover) (ctrl.Result, error) {
	if len(tf.Status.Clones) == 0 {
		return r.fail(ctx, tf, "internal: no recovery point was resolved before Cloning")
	}

	apiClient := webapi.NewClient()
	if secret, err := r.clusterSecret(ctx, tf.Namespace, tf.Spec.BubbleCluster); err == nil && secret != "" {
		ctx = webapi.WithBearerToken(ctx, secret)
	}

	// One clone per slot, on the backend the point lives on, so no data crosses a
	// cluster boundary. ensureClone is idempotent, so re-entry reuses any clone
	// already built. A volume drill has one slot; a group drill has one per member.
	handles := make([]string, len(tf.Status.Clones))
	sizes := make([]int64, len(tf.Status.Clones))
	for i := range tf.Status.Clones {
		c := tf.Status.Clones[i]
		if c.SnapshotID == "" {
			return r.fail(ctx, tf, "internal: the recovery point was not resolved for member "+c.SourceRef)
		}
		snapCluster, snapPool, snapUUID, ok := splitHandle(c.SnapshotID)
		if !ok {
			return r.fail(ctx, tf, "recovery point handle is malformed: "+c.SnapshotID)
		}
		name := testFailoverCloneName(tf)
		if tf.Spec.Scope == simplyblockv1alpha2.TestFailoverScopeGroup {
			name = testFailoverMemberName(tf, c.SourceRef, "clone")
		}
		cloneUUID, sizeBytes, err := r.ensureClone(ctx, apiClient, snapCluster, snapPool, snapUUID, name)
		if err != nil {
			return ctrl.Result{}, err
		}
		handles[i] = snapCluster + ":" + snapPool + ":" + cloneUUID
		sizes[i] = sizeBytes
	}

	if err := r.transitionTo(ctx, tf, simplyblockv1alpha2.TestFailoverStepPlacing, func(s *simplyblockv1alpha2.TestFailoverStatus) {
		for i := range s.Clones {
			s.Clones[i].CloneID = handles[i]
			if sizes[i] > 0 {
				s.Clones[i].SizeBytes = sizes[i]
			}
		}
		s.Message = fmt.Sprintf("cloned %d recovery point(s); placing on %s", len(s.Clones), tf.Spec.BubbleCluster)
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(tf, nil, corev1.EventTypeNormal, "CloneBuilt", "CloneBuilt",
		"cloned %d recovery point(s) on cluster %s, source untouched", len(tf.Status.Clones), tf.Spec.BubbleCluster)
	return ctrl.Result{Requeue: true}, nil
}

// ensureClone returns the id of the clone named name, cloning the snapshot if it
// does not exist yet. Ask-then-act: it lists the pool's volumes first, so a retry
// after a crash between clone and status-write reuses the clone rather than
// building a second (reconciler-patterns §4).
func (r *TestFailoverReconciler) ensureClone(ctx context.Context, api *webapi.Client, clusterUUID, poolID, snapUUID, name string) (string, int64, error) {
	listEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes", clusterUUID, poolID)
	body, status, err := api.Do(ctx, http.MethodGet, listEndpoint, nil)
	if err != nil || status >= 300 {
		return "", 0, requestError("list volumes", body, status, err)
	}
	var existing []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	_ = json.Unmarshal(body, &existing)
	for _, v := range existing {
		if v.Name == name {
			return v.ID, v.Size, nil
		}
	}

	createEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes?response_format=full", clusterUUID, poolID)
	body, status, err = api.Do(ctx, http.MethodPost, createEndpoint,
		map[string]interface{}{"name": name, "snapshot_id": snapUUID})
	if err != nil || status >= 300 {
		return "", 0, requestError("clone snapshot", body, status, err)
	}
	var created struct {
		ID   string `json:"id"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return "", 0, fmt.Errorf("clone snapshot: no volume id in the response")
	}
	return created.ID, created.Size, nil
}

// placeBubble delivers the bubble PV and PVC to the recovery cluster through an
// OCM ManifestWork and marks the drill Ready once the PVC binds. It is
// non-blocking: the ManifestWork is created once and its bind status awaited
// across reconciles via its status feedback.
func (r *TestFailoverReconciler) placeBubble(ctx context.Context, tf *simplyblockv1alpha2.TestFailover) (ctrl.Result, error) {
	if len(tf.Status.Clones) == 0 || tf.Status.Clones[0].CloneID == "" {
		return r.fail(ctx, tf, "internal: the clone was not built before Placing")
	}

	var mw workv1.ManifestWork
	err := r.Get(ctx, client.ObjectKey{Namespace: tf.Spec.BubbleCluster, Name: testFailoverManifestWorkName(tf)}, &mw)
	if apierrors.IsNotFound(err) {
		desired, buildErr := r.bubbleManifestWork(tf)
		if buildErr != nil {
			return ctrl.Result{}, buildErr
		}
		if createErr := r.Create(ctx, desired); createErr != nil {
			return ctrl.Result{}, createErr
		}
		return r.hold(ctx, tf, "placing the bubble PVC on cluster "+tf.Spec.BubbleCluster)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if bound := boundBubblePVCs(&mw); bound < len(tf.Status.Clones) {
		return r.hold(ctx, tf, fmt.Sprintf("waiting for the bubble PVCs to bind on cluster %s (%d of %d bound)", tf.Spec.BubbleCluster, bound, len(tf.Status.Clones)))
	}

	// The non-disruptiveness guard: the drill only ever read the source, so it
	// must still be the same volume it started from. A drill that cannot prove
	// this is a defect, not a pass.
	held := r.sourceUnchanged(ctx, tf)
	now := metav1.Now()
	if err := r.patchStatus(ctx, tf, func(s *simplyblockv1alpha2.TestFailoverStatus) {
		if s.Report == nil {
			s.Report = &simplyblockv1alpha2.TestFailoverReport{}
		}
		s.Report.InvariantsHeld = held
		if held {
			s.Phase = simplyblockv1alpha2.TestFailoverPhaseReady
			s.Message = "bubble ready: the recovered PVC is bound on cluster " + tf.Spec.BubbleCluster
			if s.ReadyAt == nil {
				s.ReadyAt = &now
			}
		} else {
			s.Phase = simplyblockv1alpha2.TestFailoverPhaseFailed
			s.Message = "the source changed during the drill; the test was not non-disruptive"
			if s.CompletedAt == nil {
				s.CompletedAt = &now
			}
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	if held {
		r.Recorder.Eventf(tf, nil, corev1.EventTypeNormal, "BubbleReady", "BubbleReady",
			"the bubble PVC is bound on cluster %s", tf.Spec.BubbleCluster)
	} else {
		r.Recorder.Eventf(tf, nil, corev1.EventTypeWarning, "InvariantViolated", "InvariantViolated",
			"the source changed during the drill")
	}
	return ctrl.Result{}, nil
}

// bubbleVolumeContextStripKeys are the source PV volumeAttributes that must NOT
// be carried onto the bubble PV: they identify the SOURCE volume and its NVMe-oF
// target. The node plugin re-resolves the clone's own identity from the clone
// handle at stage time, so these are redundant on success; on a failed clone
// lookup, a stale source NQN/connections here would silently point the mount back
// at the source (reachable across clusters on a flat network), so they are
// dropped and staging fails safe instead.
var bubbleVolumeContextStripKeys = map[string]struct{}{
	"cluster_id": {}, "pool_name": {}, "nqn": {}, "connections": {},
	"model": {}, "name": {}, "uuid": {}, "nsId": {}, "targetLvolID": {},
}

// bubbleVolumeContext copies the source PV's volumeAttributes minus the identity
// keys above and the provisioner-injected keys (csi.storage.k8s.io/*,
// storage.kubernetes.io/*), leaving the class-level parameters the node plugin
// needs. It returns nil when nothing survives, which the driver tolerates.
func bubbleVolumeContext(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		if _, strip := bubbleVolumeContextStripKeys[k]; strip {
			continue
		}
		if strings.HasPrefix(k, "csi.storage.k8s.io/") || strings.HasPrefix(k, "storage.kubernetes.io/") {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// bubbleManifestWork wraps the bubble namespace, a static PersistentVolume bound
// to the clone, and its PersistentVolumeClaim, in a ManifestWork addressed to the
// recovery cluster, with a feedback rule that reports the PVC's bind phase back to
// the hub.
func (r *TestFailoverReconciler) bubbleManifestWork(tf *simplyblockv1alpha2.TestFailover) (*workv1.ManifestWork, error) {
	ns := tf.Spec.BubbleNamespace
	labels := map[string]string{testFailoverIDLabel: string(tf.UID)}
	scName := ""
	group := tf.Spec.Scope == simplyblockv1alpha2.TestFailoverScopeGroup

	namespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: labels},
	}
	manifests := []workv1.Manifest{}
	raw, err := json.Marshal(namespace)
	if err != nil {
		return nil, fmt.Errorf("marshal bubble namespace: %w", err)
	}
	manifests = append(manifests, workv1.Manifest{RawExtension: runtime.RawExtension{Raw: raw}})

	// One PV+PVC pair per clone slot, each reporting its own bind phase back to the
	// hub. A volume drill has one; a group drill has one per member, all in the one
	// bubble namespace so the recovered set is crash-consistent.
	var configs []workv1.ManifestConfigOption
	for i := range tf.Status.Clones {
		clone := tf.Status.Clones[i]
		pvName := testFailoverPVName(tf)
		pvcName := tf.Spec.SourceRef
		if group {
			pvName = testFailoverMemberName(tf, clone.SourceRef, "pv")
			pvcName = clone.SourceRef
		}
		capacity := *resource.NewQuantity(clone.SizeBytes, resource.BinarySI)

		pv := &corev1.PersistentVolume{
			TypeMeta:   metav1.TypeMeta{Kind: "PersistentVolume", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{Name: pvName, Labels: labels},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:                      corev1.ResourceList{corev1.ResourceStorage: capacity},
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				StorageClassName:              scName,
				ClaimRef: &corev1.ObjectReference{
					Kind: "PersistentVolumeClaim", APIVersion: "v1", Namespace: ns, Name: pvcName,
				},
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:           csiDriverName,
						VolumeHandle:     clone.CloneID,
						FSType:           clone.SourceFSType,
						VolumeAttributes: clone.SourceVolumeContext,
					},
				},
			},
		}
		pvc := &corev1.PersistentVolumeClaim{
			TypeMeta:   metav1.TypeMeta{Kind: "PersistentVolumeClaim", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: ns, Labels: labels},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: capacity}},
				StorageClassName: &scName,
				VolumeName:       pvName,
			},
		}
		for _, obj := range []client.Object{pv, pvc} {
			raw, err := json.Marshal(obj)
			if err != nil {
				return nil, fmt.Errorf("marshal bubble manifest: %w", err)
			}
			manifests = append(manifests, workv1.Manifest{RawExtension: runtime.RawExtension{Raw: raw}})
		}
		configs = append(configs, workv1.ManifestConfigOption{
			ResourceIdentifier: workv1.ResourceIdentifier{
				Group: "", Resource: "persistentvolumeclaims", Namespace: ns, Name: pvcName,
			},
			FeedbackRules: []workv1.FeedbackRule{{
				Type:      workv1.JSONPathsType,
				JsonPaths: []workv1.JsonPath{{Name: "phase", Path: ".status.phase"}},
			}},
		})
	}

	return &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testFailoverManifestWorkName(tf),
			Namespace: tf.Spec.BubbleCluster,
			Labels:    labels,
		},
		Spec: workv1.ManifestWorkSpec{
			Workload:        workv1.ManifestsTemplate{Manifests: manifests},
			ManifestConfigs: configs,
		},
	}, nil
}

// boundBubblePVCs counts the bubble PVCs the ManifestWork's status feedback
// reports Bound. Placing is complete only when every clone's PVC is bound.
func boundBubblePVCs(mw *workv1.ManifestWork) int {
	bound := 0
	for _, m := range mw.Status.ResourceStatus.Manifests {
		if m.ResourceMeta.Resource != "persistentvolumeclaims" {
			continue
		}
		for _, v := range m.StatusFeedbacks.Values {
			if v.Name == "phase" && v.Value.String != nil && *v.Value.String == string(corev1.ClaimBound) {
				bound++
				break
			}
		}
	}
	return bound
}

// testFailoverPVName is the deterministic name of the drill's static
// PersistentVolume on the recovery cluster.
func testFailoverPVName(tf *simplyblockv1alpha2.TestFailover) string {
	h := sha256.Sum256([]byte(tf.Namespace + "/" + tf.Name))
	return fmt.Sprintf("tfo-%x-pv", h[:6])
}

// testFailoverManifestWorkName is the deterministic name of the drill's
// ManifestWork in the recovery cluster's namespace on the hub.
func testFailoverManifestWorkName(tf *simplyblockv1alpha2.TestFailover) string {
	h := sha256.Sum256([]byte(tf.Namespace + "/" + tf.Name))
	return fmt.Sprintf("tfo-%x-bubble", h[:6])
}

// clusterSecret returns the cluster secret the control plane authenticates a
// per-cluster call with, from the hub Secret simplyblock-cluster-<name>.
func (r *TestFailoverReconciler) clusterSecret(ctx context.Context, namespace, clusterName string) (string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "simplyblock-cluster-" + clusterName}, &secret); err != nil {
		return "", err
	}
	return string(secret.Data["secret"]), nil
}

// testFailoverSnapshotName is the deterministic name of the snapshot an in-place
// drill takes, so ask-then-act can find it on a retry.
func testFailoverSnapshotName(tf *simplyblockv1alpha2.TestFailover) string {
	h := sha256.Sum256([]byte(tf.Namespace + "/" + tf.Name))
	return fmt.Sprintf("tfo-%x-snap", h[:6])
}

// testFailoverCloneName is the deterministic name of the clone a drill builds, so
// ask-then-act can find it on a retry.
func testFailoverCloneName(tf *simplyblockv1alpha2.TestFailover) string {
	h := sha256.Sum256([]byte(tf.Namespace + "/" + tf.Name))
	return fmt.Sprintf("tfo-%x-clone", h[:6])
}

// testFailoverMemberName is the deterministic name of one group member's drill
// object (clone or PV), keyed by the drill and the member's source ref, so a
// group drill's members do not collide and each is re-findable on a retry.
func testFailoverMemberName(tf *simplyblockv1alpha2.TestFailover, memberRef, kind string) string {
	h := sha256.Sum256([]byte(tf.Namespace + "/" + tf.Name))
	m := sha256.Sum256([]byte(memberRef))
	return fmt.Sprintf("tfo-%x-%x-%s", h[:6], m[:4], kind)
}

// splitHandle splits a CSI handle "cluster:pool:uuid" into its three parts.
func splitHandle(handle string) (cluster, pool, uuid string, ok bool) {
	parts := strings.Split(handle, ":")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// lastPathSegment returns the final segment of a URL path.
func lastPathSegment(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// requestError builds an error for a failed control-plane call.
func requestError(what string, body []byte, status int, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: status %d: %s", what, status, string(body))
}

// SetupWithManager registers the controller.
func (r *TestFailoverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.TestFailover{}).
		Owns(&workv1.ManifestWork{}).
		Named("testfailover").
		Complete(r)
}
