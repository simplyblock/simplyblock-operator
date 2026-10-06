// The consistency-group membership watcher (design §4.5, Phase 4): a PVC
// informer in the controller plugin that makes the membership label live for
// the volume's whole life. A label added to an existing PVC joins its volume
// to the named group, and a removed label detaches it. The CSI specification
// has no verb for a label change, so this lives beside the provisioner rather
// than behind a CSI RPC. The watcher relays the label and the backend decides:
// it holds no membership state of its own, and a refused join is surfaced as a
// Warning event on the PVC and retried on resync rather than hot-looped.
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// membershipResync is how often every PVC is re-reconciled. It is the retry
// loop for standing mismatches (a group that does not exist yet, a refused
// join whose precondition may clear), so it is minutes, not hours.
const membershipResync = 5 * time.Minute

// membershipClient is the slice of the control-plane surface the watcher
// needs. *controlplane.ClusterClient satisfies it, and tests stub it.
type membershipClient interface {
	GetVolumeGroupID(ctx context.Context, lvolID string) (string, error)
	ResolveConsistencyGroupByName(ctx context.Context, name string) (*controlplane.ConsistencyGroupSummary, error)
	GetConsistencyGroup(ctx context.Context, groupID string) (*controlplane.ConsistencyGroupSummary, error)
	JoinConsistencyGroupMember(ctx context.Context, groupID, lvolID string) error
	DetachConsistencyGroupMember(ctx context.Context, groupID, lvolID string) error
	PlanConsistencyGroupJoin(ctx context.Context, groupID, lvolID string) (*controlplane.JoinPlan, error)
	ColocateConsistencyGroupMember(ctx context.Context, groupID, lvolID string, clientSwapReady bool) error
}

// migrationRequester asks the operator to live-migrate a PersistentVolume's
// backing volume to a storage node, through a VolumeMigration: the operator
// attaches the target paths on the consumer host before the backend moves the
// data, which only it can do. Ensure creates the request once (by name) and
// reports its phase; "" while the operator has not picked it up.
type migrationRequester interface {
	Ensure(ctx context.Context, name, pvName, targetNodeUUID string) (string, error)
}

// cgMembershipWatcher reconciles PVC label state to backend group membership.
type cgMembershipWatcher struct {
	kube       kubernetes.Interface
	driverName string
	// clientFor resolves the control-plane client for a volume's cluster and
	// pool; production wires clusters.Client, tests substitute a stub.
	clientFor func(ctx context.Context, clusterID, poolRef string) (membershipClient, error)
	recorder  record.EventRecorder
	// migrations runs the pre-join live migration of a volume off its group's
	// pinned node (co-location design §5); nil disables it, and an off-pin
	// join stays refused as before.
	migrations migrationRequester
	// colocate moves a member into its group's subsystem after the join; off
	// by default, because the backend refuses the move for an attached volume
	// until the node plugin swaps paths (design §6).
	colocate bool
	// clientSwapReady asserts every node stages volumes behind the
	// device-mapper indirection and swaps paths itself (design §6).
	clientSwapReady bool
}

// StartConsistencyGroupLabelWatcher runs the membership watcher until ctx is
// canceled. It is a no-op (with a log line) when kube is nil, mirroring how
// the annotation helpers degrade without an in-cluster config.
//
// dyn, when non-nil, lets the watcher request the pre-join live migration of a
// volume off its group's pinned node (a VolumeMigration); without it an
// off-pin join stays refused.
func StartConsistencyGroupLabelWatcher(
	ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface, driverName string,
) {
	if kube == nil {
		klog.Warning("consistency-group label watcher disabled: no Kubernetes client")
		return
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	watcher := &cgMembershipWatcher{
		kube:       kube,
		driverName: driverName,
		clientFor: func(ctx context.Context, clusterID, poolRef string) (membershipClient, error) {
			return clusters.Client(ctx, clusterID, poolRef)
		},
		recorder: broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "spdkcsi-cg-membership"}),
	}
	preJoin, colocate, swapReady := watcherOptions()
	if preJoin && dyn != nil {
		watcher.migrations = newVolumeMigrations(dyn)
	}
	watcher.colocate = colocate
	watcher.clientSwapReady = swapReady

	factory := informers.NewSharedInformerFactory(kube, membershipResync)
	informer := factory.Core().V1().PersistentVolumeClaims().Informer()
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) { watcher.handle(ctx, obj) },
		UpdateFunc: func(_, newObj interface{}) {
			watcher.handle(ctx, newObj)
		},
		// Deletion needs no handler: deleting the PVC deletes the volume, and
		// the volume delete path settles membership (design §8.2).
	})
	if err != nil {
		klog.Errorf("consistency-group label watcher disabled: %v", err)
		return
	}
	factory.Start(ctx.Done())
	klog.Infof("consistency-group label watcher started (resync %s)", membershipResync)
}

func (w *cgMembershipWatcher) handle(ctx context.Context, obj interface{}) {
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok {
		return
	}
	if err := w.reconcile(ctx, pvc); err != nil {
		// Transient (backend unreachable, PV read failed): the resync is the
		// retry. Refusals and holds are events, not errors, and land below.
		klog.Warningf("consistency-group membership reconcile of PVC %s/%s: %v",
			pvc.Namespace, pvc.Name, err)
	}
}

// reconcile converges ONE PVC's label with backend membership. It returns an
// error only for transient faults worth logging; every deliberate hold or
// refusal is an event on the PVC, because a label that silently does nothing
// is indistinguishable from a stalled controller (design §4.5).
func (w *cgMembershipWatcher) reconcile(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		return nil
	}
	pv, err := w.kube.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read PV %s: %w", pvc.Spec.VolumeName, err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != w.driverName {
		return nil
	}
	handle, err := csicommon.ParseVolumeHandle(pv.Spec.CSI.VolumeHandle)
	if err != nil {
		return nil // not a volume this driver can address; nothing to reconcile
	}
	client, err := w.clientFor(ctx, handle.ClusterID, handle.PoolRef)
	if err != nil {
		return fmt.Errorf("control-plane client for cluster %s: %w", handle.ClusterID, err)
	}

	label := pvc.Labels[consistencyGroupLabel]
	groupID, err := client.GetVolumeGroupID(ctx, handle.VolumeID)
	if err != nil {
		return fmt.Errorf("read group membership of volume %s: %w", handle.VolumeID, err)
	}

	switch {
	case label != "" && groupID == "":
		return w.join(ctx, client, pvc, pv.Name, label, handle.VolumeID)
	case label == "" && groupID != "":
		return w.detach(ctx, client, pvc, groupID, handle.VolumeID)
	case label != "" && groupID != "":
		// Both set: converged when the names agree; a conflict is surfaced,
		// never acted on, because moving a volume between groups would be a
		// detach plus a one-way-forbidden rejoin (design §4.3).
		group, err := client.GetConsistencyGroup(ctx, groupID)
		if err != nil {
			return fmt.Errorf("read consistency group %s: %w", groupID, err)
		}
		if group.Name != "" && group.Name != label {
			w.recorder.Eventf(pvc, corev1.EventTypeWarning, "ConsistencyGroupConflict",
				"volume is a member of consistency group %q but the PVC is labeled %q; "+
					"membership is one-way, so relabeling cannot move a volume between groups",
				group.Name, label)
			return nil
		}
		return w.colocateMember(ctx, client, pvc, groupID, label, handle.VolumeID)
	}
	return nil
}

// preJoinMigrationName is the VolumeMigration a late join requests for a
// volume: one per volume, so a resync finds the request it already made.
func preJoinMigrationName(lvolID string) string { return "cg-join-" + lvolID }

func (w *cgMembershipWatcher) join(
	ctx context.Context, client membershipClient,
	pvc *corev1.PersistentVolumeClaim, pvName, label, lvolID string,
) error {
	group, err := client.ResolveConsistencyGroupByName(ctx, label)
	if err != nil {
		return fmt.Errorf("resolve consistency group %q: %w", label, err)
	}
	if group == nil {
		// A group is born from its first provisioned labeled volume (§4.1);
		// the watcher never creates one. Hold, visibly, until it exists.
		w.recorder.Eventf(pvc, corev1.EventTypeNormal, "ConsistencyGroupPending",
			"consistency group %q does not exist yet; it is created by the first volume provisioned with the label", label)
		return nil
	}
	if err := client.JoinConsistencyGroupMember(ctx, group.ID, lvolID); err != nil {
		if errors.Is(err, controlplane.ErrMembershipRefused) {
			return w.joinRefused(ctx, client, pvc, pvName, label, group.ID, lvolID, err)
		}
		return fmt.Errorf("join consistency group %q: %w", label, err)
	}
	w.recorder.Eventf(pvc, corev1.EventTypeNormal, "ConsistencyGroupJoined",
		"volume joined consistency group %q; it is included from the next generation", label)
	return w.colocateMember(ctx, client, pvc, group.ID, label, lvolID)
}

// joinRefused turns a refused join into the pre-join migration when the only
// obstacle is placement: the backend's plan says the volume (with its
// subsystem siblings) must first move to the group's pinned node. Anything
// else stays a refusal on the PVC.
func (w *cgMembershipWatcher) joinRefused(
	ctx context.Context, client membershipClient, pvc *corev1.PersistentVolumeClaim,
	pvName, label, groupID, lvolID string, refusal error,
) error {
	refuse := func(err error) error {
		w.recorder.Eventf(pvc, corev1.EventTypeWarning, "ConsistencyGroupJoinRefused",
			"volume cannot join consistency group %q: %v", label, err)
		return nil
	}
	if w.migrations == nil {
		return refuse(refusal)
	}
	plan, err := client.PlanConsistencyGroupJoin(ctx, groupID, lvolID)
	if err != nil {
		if errors.Is(err, controlplane.ErrMembershipRefused) {
			return refuse(err)
		}
		return fmt.Errorf("plan the join to consistency group %q: %w", label, err)
	}
	if !plan.Has(controlplane.JoinStepMigrate) || plan.TargetNodeID == "" {
		return refuse(refusal)
	}
	name := preJoinMigrationName(lvolID)
	phase, err := w.migrations.Ensure(ctx, name, pvName, plan.TargetNodeID)
	if err != nil {
		return fmt.Errorf("request the pre-join migration of %s: %w", pvName, err)
	}
	switch phase {
	case migrationCompleted:
		// Moved: the placement precondition holds now. The resync or the next
		// PVC event joins; joining here would race the backend's record switch
		// of the last subsystem sibling.
		w.recorder.Eventf(pvc, corev1.EventTypeNormal, "ConsistencyGroupMigrated",
			"volume moved to the group's node %s; joining consistency group %q on the next pass",
			plan.TargetNodeID, label)
	case migrationFailed, migrationAborted:
		w.recorder.Eventf(pvc, corev1.EventTypeWarning, "ConsistencyGroupMigrationFailed",
			"the migration to consistency group %q's node %s ended %s; delete VolumeMigration %s to retry",
			label, plan.TargetNodeID, phase, name)
	default:
		w.recorder.Eventf(pvc, corev1.EventTypeNormal, "ConsistencyGroupMigrating",
			"volume is off consistency group %q's node: migrating it (with %d volume(s) of its "+
				"subsystem) to node %s through VolumeMigration %s before the join", label,
			len(plan.MigrateLvolIDs), plan.TargetNodeID, name)
	}
	return nil
}

// The VolumeMigration phases the watcher acts on (operator api/v1alpha1).
const (
	migrationCompleted = "Completed"
	migrationFailed    = "Failed"
	migrationAborted   = "Aborted"
)

// colocateMember moves a current member into its group's subsystem when the
// watcher is configured to; a refusal (moves disabled, or a connected host and
// no client swap) is a Normal event, since the member is valid where it is.
func (w *cgMembershipWatcher) colocateMember(
	ctx context.Context, client membershipClient, pvc *corev1.PersistentVolumeClaim,
	groupID, label, lvolID string,
) error {
	if !w.colocate {
		return nil
	}
	err := client.ColocateConsistencyGroupMember(ctx, groupID, lvolID, w.clientSwapReady)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, controlplane.ErrColocationRefused):
		w.recorder.Eventf(pvc, corev1.EventTypeNormal, "ConsistencyGroupColocationDeferred",
			"volume stays in its own subsystem for now (consistency group %q): %v", label, err)
		return nil
	default:
		return fmt.Errorf("co-locate with consistency group %q: %w", label, err)
	}
}

func (w *cgMembershipWatcher) detach(
	ctx context.Context, client membershipClient,
	pvc *corev1.PersistentVolumeClaim, groupID, lvolID string,
) error {
	group, err := client.GetConsistencyGroup(ctx, groupID)
	if err != nil {
		return fmt.Errorf("read consistency group %s: %w", groupID, err)
	}
	if group.Name == "" {
		// A legacy policy-owned group: its membership is the policy's, not the
		// label's, so a label-less PVC says nothing about it.
		return nil
	}
	if err := client.DetachConsistencyGroupMember(ctx, groupID, lvolID); err != nil {
		return fmt.Errorf("detach from consistency group %q: %w", group.Name, err)
	}
	w.recorder.Eventf(pvc, corev1.EventTypeNormal, "ConsistencyGroupDetached",
		"volume left consistency group %q; generations that contain it stay restorable", group.Name)
	return nil
}
