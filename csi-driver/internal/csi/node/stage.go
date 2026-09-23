// Staging and unstaging: bringing a volume's stack up on this node, taking it
// back down, and repairing one whose foundation went away underneath it.
//
// Each of the three is one runner call against a plan. Which plan a volume is
// lives in plan.go, what the plan is walked with lives in stack.go, and what
// each layer does lives in atlas-lib. What is left here is the order the RPCs
// impose: which verb, against which plan, and what the node has to remember
// once it is done.
//
// The separation of Release and Destroy is the load-bearing part. An unstage
// fires whenever no pod on this node needs the volume mounted, which includes
// an ordinary pod restart, so it releases and never destroys.

package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
	"github.com/simplyblock/atlas/volstack/plans"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/initiator"
)

// teardownTimeout bounds a release that the RPC's own context may already have
// canceled. A teardown that stops halfway leaves paths attached that nothing
// will come back for.
const teardownTimeout = 2 * time.Minute

func (ns *Server) NodeStageVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	stagingParentPath := req.GetStagingTargetPath() // where the volume context is stashed
	stagingTargetPath := getStagingTargetPath(req)

	isStaged, err := ns.mounter.IsMounted(stagingTargetPath)
	if err != nil {
		klog.Errorf("failed to check isStaged, targetPath: %s err: %v", stagingTargetPath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if isStaged {
		// A staged volume whose backing NVMe-oF device was lost leaves a dead
		// (EIO) mount that isStaged still reports as staged. Repair it in place
		// instead of short-circuiting.
		if !ns.mounter.IsDead(stagingTargetPath) {
			klog.Warning("volume already staged")
			return &csi.NodeStageVolumeResponse{}, nil
		}
		klog.Warningf("volume %s already staged but its mount is dead; restaging", volumeID)
		if err := ns.restageVolume(ctx, volumeID, stagingTargetPath, stagingParentPath, req.GetVolumeCapability()); err != nil { //nolint:lll // unwrappable string/log/signature
			return nil, status.Errorf(codes.Internal, "restage volume %s: %v", volumeID, err)
		}
		return &csi.NodeStageVolumeResponse{}, nil
	}

	vc := req.GetVolumeContext()
	vc["stagingParentPath"] = stagingParentPath
	ns.refreshVolumeContext(ctx, volumeID, vc)

	plan, err := ns.attachPlan(ctx, volumeID, stagingTargetPath, vc, req.GetVolumeCapability())
	if err != nil {
		klog.Errorf("failed to build the stack plan, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	artifact, err := ns.bringUp(ctx, volumeID, plan, vc)
	if err != nil {
		klog.Errorf("failed to bring up the stack, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	ns.rememberStagedVolume(ctx, volumeID, vc, artifact, req.GetVolumeCapability())

	// The CSI spec passes VolumeContext to this RPC and to nothing after it, so
	// what the later RPCs need is written beside the staging path.
	if err := stashVolumeContext(vc, stagingParentPath); err != nil {
		klog.Errorf("failed to stash volume context, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *Server) NodeUnstageVolume(
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest,
) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	stagingParentPath := req.GetStagingTargetPath()
	stagingTargetPath := getStagingTargetPath(req)

	volumeContext, err := lookupVolumeContext(stagingParentPath)
	if err != nil {
		// Not fatal on its own. The stack record was written before the first
		// side effect and may still name the volume's namespace, which is all a
		// release needs, and teardownPlan refuses when neither source does.
		klog.Warningf("volume %s has no stashed context; releasing it from its stack record: %v", volumeID, err)
		volumeContext = map[string]string{}
	}

	plan, err := ns.teardownPlan(volumeID, stagingTargetPath, volumeContext)
	if err != nil {
		klog.Errorf("failed to read what was staged for %s: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	// The RPC's context may already be canceled by the time a teardown reaches
	// the fabric, and a half-released stack is worse than a slow one.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
	defer cleanupCancel()

	devicePath := volumeContext["devicePath"]
	if err := ns.stack.runner.Down(cleanupCtx, volumeID, plan); err != nil {
		klog.Errorf("failed to release the stack, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if devicePath != "" {
		// A device this node tore down itself is forgotten rather than left to
		// be reported as one that vanished on its own.
		initiator.ForgetDevice(devicePath)
	}

	if ns.volumeIsBeingDeleted(ctx, volumeID) {
		// The only caller of Destroy. A volume that is going away for good takes
		// the node-local objects its stack built with it, because nothing will
		// stage it again and nothing else on this host knows they are there.
		if err := ns.stack.runner.Destroy(cleanupCtx, volumeID, plan); err != nil {
			klog.Errorf("failed to destroy the stack of the deleted volume %s: %v", volumeID, err)
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	if err := ns.mounter.Remove(stagingTargetPath); err != nil { // idempotent
		klog.Errorf("failed to delete mount point, targetPath: %s err: %v", stagingTargetPath, err)
		return nil, status.Errorf(codes.Internal, "unstage volume %s failed: %s", volumeID, err)
	}
	if err := cleanUpVolumeContext(stagingParentPath); err != nil {
		klog.Errorf("failed to clean up volume context, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// bringUp walks the plan up, and tries once more when a fabric repair changed
// something that could make the second attempt answer differently.
//
// Up itself releases what it already brought up when a layer fails, and never
// destroys: a format that failed must not trigger the removal of the object
// underneath it.
//
// The retry exists because a connect can succeed at one layer of the NVMe
// object tree while the layer below it is unusable. A subsystem whose
// controllers are live and which exports no namespace at all satisfies every
// check the connect makes, produces no block device, and is retried by kubelet
// forever, since nothing between those retries changes. The repair is the thing
// that changes something, and it reports whether it did: a repair that tore
// nothing down is no reason to run the same attach twice.
func (ns *Server) bringUp(
	ctx context.Context,
	volumeID string,
	plan volstack.Plan,
	vc map[string]string,
) (volstack.Artifact, error) {
	artifact, err := ns.stack.runner.Up(ctx, volumeID, plan)
	if err == nil || ns.repairFabric == nil {
		return artifact, err
	}
	if !ns.repairFabric(ctx, vc["nqn"], namespaceID(vc)) {
		return artifact, err
	}
	klog.Infof("volume %s: retrying the bring-up after a fabric repair", volumeID)
	return ns.stack.runner.Up(ctx, volumeID, plan)
}

// restageVolume repairs a live stack whose foundation went away underneath it,
// which is what total NVMe-oF path loss leaves behind: the kernel removes the
// device, and the filesystem above it becomes a mount that answers EIO.
//
// It heals and never brings up. The data already exists, so no layer here may
// create or format anything, which is the whole difference between this and a
// stage.
func (ns *Server) restageVolume(
	ctx context.Context,
	volumeID, stagingTargetPath, stagingParentPath string,
	volCap *csi.VolumeCapability,
) error {
	volumeContext, err := lookupVolumeContext(stagingParentPath)
	if err != nil {
		return fmt.Errorf("lookup volume context: %w", err)
	}

	plan, err := ns.attachPlan(ctx, volumeID, stagingTargetPath, volumeContext, volCap)
	if err != nil {
		return err
	}
	return ns.healStack(ctx, volumeID, plan, stagingParentPath)
}

// healStack repairs the layers that report themselves unhealthy and records the
// device the repaired stack now exposes.
//
// It takes the plan rather than building one, because a publish heals and then
// bind-mounts from the same stack and resolving where the volume is published
// twice in one RPC is one control-plane round trip too many.
func (ns *Server) healStack(
	ctx context.Context,
	volumeID string,
	plan volstack.Plan,
	stagingParentPath string,
) error {
	volumeContext, err := lookupVolumeContext(stagingParentPath)
	if err != nil {
		return fmt.Errorf("lookup volume context: %w", err)
	}
	if err := ns.stack.runner.Heal(ctx, volumeID, plan); err != nil {
		return err
	}

	// The device behind a healed stack is a new one: a reconnect produces a
	// different namespace device, and the later RPCs act on what is recorded
	// here.
	artifact, err := ns.stack.runner.Observe(ctx, plan)
	if err != nil {
		return err
	}
	if device, ok := artifact.Device(); ok {
		volumeContext["devicePath"] = device.Path
		initiator.MarkDevicePresent(device.Path, deviceLvolID(volumeContext))
	}
	if err := stashVolumeContext(volumeContext, stagingParentPath); err != nil {
		klog.Warningf("healStack: failed to re-stash volume context for %s: %v", volumeID, err)
	}
	klog.Infof("healed the stack of volume %s", volumeID)
	return nil
}

// attachPlan is the plan for an operation that may attach: a stage or a heal.
//
// Where the volume is published is resolved from the control plane rather than
// from the stashed context, because the context cannot carry what a connect
// needs. A DHCHAP key is a credential and the stash outlives the pod that wrote
// it, so the secrets are re-read here and held only in memory.
func (ns *Server) attachPlan(
	ctx context.Context,
	volumeID, stagingTargetPath string,
	vc map[string]string,
	volCap *csi.VolumeCapability,
) (volstack.Plan, error) {
	connection, hostNQN, err := ns.publishedConnection(ctx, volumeID, vc)
	if err != nil {
		return nil, err
	}
	node := ns.stack.node(hostNQN, ns.priorFormat(volumeID, vc))
	volume := stackVolume(stagingTargetPath, vc, volCap)
	return planFor(node, connection, volume, vdoOptions(vc), shapeFor(vc, volCap)), nil
}

// teardownPlan is the plan an unstage walks, which is the shape that was built
// rather than the shape the volume's class describes now.
//
// A class can be edited or deleted after a volume is provisioned, and a
// teardown owes the truth about what is on the host. An absent record means the
// volume was staged by a version predating the stack, and what that version
// built is fabric → filesystem.
func (ns *Server) teardownPlan(
	volumeID, stagingTargetPath string,
	vc map[string]string,
) (volstack.Plan, error) {
	shape, fsType := shapePlain, ""
	options := vdoOptions(vc)

	// Declared outside the switch: an absent record is not a failure, and the
	// zero value is then what the connection below is merged with.
	var record volstack.Record
	var err error

	record, err = ns.stack.store.Load(volumeID)
	switch {
	case errors.Is(err, volstack.ErrNoRecord):
		klog.Infof("volume %s has no stack record; releasing it as fabric → filesystem", volumeID)
	case err != nil:
		// A record that cannot be read is not the same as one that is absent: a
		// teardown driven by a misread plan releases the wrong objects.
		return nil, err
	default:
		if shape, err = shapeFromRecord(recordedLayers(record)); err != nil {
			return nil, err
		}
		fsType = recordedFsType(record)
		if recorded, ok := recordedLVMOptions(record); ok {
			options = recorded
		}
	}

	if fsType != "" {
		vc[stagedFsTypeKey] = fsType
	}
	connection, err := teardownConnection(record, vc)
	if err != nil {
		return nil, err
	}

	node := ns.stack.node("", nil)
	volume := stackVolume(stagingTargetPath, vc, nil)
	return planFor(node, connection, volume, options, shape), nil
}

// teardownConnection identifies the namespace a release acts on, from the
// stashed context and from the record where the context falls short.
//
// The record is the second source because it survives what the context does
// not: a crash between the mount and the stash leaves a stack that is up and a
// context that was never written, and the record was written before the first
// side effect precisely so that such a stack is still removable.
//
// A connection that identifies nothing is refused. The fabric layer looks its
// device up by the identity it is handed, and a selector that constrains
// nothing matches every namespace attached to the node, so releasing on one
// would detach another volume.
func teardownConnection(record volstack.Record, vc map[string]string) (lvol.Connection, error) {
	connection := connectionFromContext(vc)

	if params, ok := recordedFabric(record); ok {
		if connection.NQN == "" {
			connection.NQN = params.NQN
		}
		if connection.NSID == 0 {
			connection.NSID = params.NSID
		}
	}

	if connection.UUID == "" && connection.NQN == "" {
		return lvol.Connection{}, errors.New(
			"neither the stashed volume context nor the stack record names this volume's namespace, " +
				"and releasing a namespace nothing identifies would detach whichever one was found first")
	}
	return connection, nil
}

// recordedFabric is the parameters of the record's bottom fabric layer, and
// reports whether it has one that can be read.
func recordedFabric(record volstack.Record) (layers.FabricParams, bool) {
	for _, entry := range record.Plan {
		if entry.Layer != layerFabric || len(entry.Params) == 0 {
			continue
		}
		var params layers.FabricParams
		if err := json.Unmarshal(entry.Params, &params); err != nil {
			return layers.FabricParams{}, false
		}
		return params, true
	}
	return layers.FabricParams{}, false
}

// recordedLVMOptions is what the record says the logical volume was created as,
// and reports whether it names one at all.
//
// A teardown takes the pool's name from here rather than from the volume's
// class, for the same reason it takes the layer list from here: the class can
// have been edited since, and a release pointed at a pool by another name finds
// nothing to release. Which is also why an unreadable entry is not fatal — the
// verbs a teardown reaches never consult the definition, and refusing the whole
// release over a field none of them reads would strand the volume's objects.
func recordedLVMOptions(record volstack.Record) (plans.LogicalVolumeOptions, bool) {
	for _, entry := range record.Plan {
		if entry.Layer != layerLVMLogicalVolume || len(entry.Params) == 0 {
			continue
		}
		var params layers.LVMLogicalVolumeParams
		if err := json.Unmarshal(entry.Params, &params); err != nil {
			return plans.LogicalVolumeOptions{}, false
		}
		return plans.LogicalVolumeOptions{
			Definition: lvm.LogicalVolumeDefinition{
				Deduplication:    params.Deduplication,
				Compression:      params.Compression,
				Stripes:          params.Stripes,
				StripeChunkBytes: params.StripeChunkBytes,
			},
			PoolName: params.PoolName,
		}, true
	}
	return plans.LogicalVolumeOptions{}, false
}

// recordedLayers is the layer list a record names, in order.
func recordedLayers(record volstack.Record) []string {
	names := make([]string, 0, len(record.Plan))
	for _, entry := range record.Plan {
		names = append(names, entry.Layer)
	}
	return names
}

// recordedFsType is the filesystem the record says was put on the volume, and
// the empty string when the record names no filesystem layer or its parameters
// cannot be read. Nothing a release does depends on it, so an unreadable one is
// not a reason to refuse the teardown.
func recordedFsType(record volstack.Record) string {
	for _, entry := range record.Plan {
		if entry.Layer != layerFilesystem || len(entry.Params) == 0 {
			continue
		}
		var params layers.FilesystemParams
		if err := json.Unmarshal(entry.Params, &params); err != nil {
			return ""
		}
		return params.FsType
	}
	return ""
}

// publishedConnection resolves where the volume is currently published.
//
// The control plane is asked first, because it is the only party that resolves
// a host's DHCHAP key material and because a volume that failed over is served
// out of a clone the stashed context does not name. The context is the fallback
// and is enough to find and release a device that is already attached, which is
// what matters when the control plane is unreachable.
func (ns *Server) publishedConnection(
	ctx context.Context,
	volumeID string,
	vc map[string]string,
) (lvol.Connection, string, error) {
	handle, err := csicommon.ParseVolumeHandle(volumeID)
	if err != nil {
		klog.Warningf("volume %s does not carry a simplyblock handle: %v", volumeID, err)
		handle = nil
	}

	if connection, hostNQN, ok := ns.controlPlaneConnection(ctx, vc, handle); ok {
		return connection, hostNQN, nil
	}

	connection := connectionFromContext(vc)
	if connection.NQN == "" {
		return lvol.Connection{}, "", fmt.Errorf(
			"volume %s: the control plane did not answer and its stashed context names no subsystem, "+
				"so there is nothing to attach", volumeID)
	}
	klog.Warningf("volume %s: attaching from the stashed context, because the control plane did not answer",
		volumeID)
	return connection, vc["hostNQN"], nil
}

// controlPlaneConnection asks the control plane where the volume lives,
// reporting whether it answered. A refusal is not an error here: the caller has
// a weaker answer to fall back on and decides what a missing one means.
func (ns *Server) controlPlaneConnection(
	ctx context.Context,
	vc map[string]string,
	handle *lvol.Handle,
) (lvol.Connection, string, bool) {
	client, err := clusters.Client(ctx, clusterIDFor(vc, handle), poolIDFor(vc, handle))
	if err != nil {
		klog.Warningf("no control-plane client for the volume's cluster: %v", err)
		return lvol.Connection{}, "", false
	}

	responses, err := client.LvolConnections(ctx, controlPlaneLvolID(vc, handle), vc["hostNQN"])
	if err != nil || len(responses) == 0 {
		klog.Warningf("the control plane published no endpoint for the volume: %v", err)
		return lvol.Connection{}, "", false
	}

	connection, hostNQN := connectionFromResponses(responses, deviceLvolID(vc))
	return connection, hostNQN, true
}

// controlPlaneLvolID is the volume the control-plane calls name, which is the
// source volume even after a failover: the backend redirects to the clone
// itself, and asking for the clone by name is what fails once the source is
// gone.
func controlPlaneLvolID(vc map[string]string, handle *lvol.Handle) string {
	if uuid := vc["uuid"]; uuid != "" {
		return uuid
	}
	if handle != nil {
		return handle.VolumeID
	}
	return ""
}

// refreshVolumeContext brings the volume context up to date with what the
// control plane says about the volume, which two cases need.
//
// A volume provisioned against a pool with allowed_hosts carries an empty NQN
// and transport type until a host is named, and a volume that may have failed over
// needs the refresh so the backend can redirect to the clone and answer with
// the namespace the data is actually being served from.
func (ns *Server) refreshVolumeContext(ctx context.Context, volumeID string, vc map[string]string) {
	if ns.kubeClient != nil {
		nodeName := ns.Driver.GetNodeID()
		node, nodeErr := ns.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if nodeErr == nil {
			vc["hostNQN"] = nqn.Host(string(node.UID))
		} else {
			klog.Warningf("failed to get node %s for hostNQN: %v", nodeName, nodeErr)
		}
	}

	spdkVol, err := csicommon.ParseVolumeHandle(volumeID)
	if err != nil {
		return
	}
	vc["poolID"] = spdkVol.PoolRef

	sbcClient, clientErr := clusters.Client(ctx, spdkVol.ClusterID, spdkVol.PoolRef)
	if clientErr != nil {
		return
	}

	connInfo, infoErr := sbcClient.VolumeInfo(ctx, spdkVol.VolumeID, vc["hostNQN"])
	if infoErr != nil {
		if errors.Is(infoErr, controlplane.ErrVolumeNotFound) {
			// The source volume was deleted by a migration with --delete-source.
			// The replication relationship survives it and names the active
			// volume on the target cluster, which is what this redirects to.
			connInfo = ns.redirectToActiveVolume(ctx, sbcClient, spdkVol.VolumeID, volumeID, vc)
		}
		if connInfo == nil {
			klog.Warningf("failed to fetch volume connection info for %s: %v", volumeID, infoErr)
		}
	}
	for k, v := range connInfo {
		vc[k] = v
	}
}

// rememberStagedVolume records what a successful bring-up produced: the device
// the later RPCs act on, its presence for the path-loss watch, and the
// filesystem the volume now carries.
//
// None of it can fail the stage. The volume is up by the time this runs, and a
// note that could not be written is a lost note rather than a broken mount.
func (ns *Server) rememberStagedVolume(
	ctx context.Context,
	volumeID string,
	vc map[string]string,
	artifact volstack.Artifact,
	volCap *csi.VolumeCapability,
) {
	if device, ok := artifact.Device(); ok {
		vc["devicePath"] = device.Path
		// Registered here rather than at the next poll of the connection
		// monitor, because a volume that attaches and loses every path inside
		// one poll interval would otherwise never have been seen as present and
		// its loss would go unnoticed.
		initiator.MarkDevicePresent(device.Path, deviceLvolID(vc))
	}
	if volCap.GetBlock() != nil {
		return
	}

	// The device carries this filesystem, because the layer either put it there
	// or refused to stage a device carrying another.
	fsType := stagedFsType(vc, volCap)
	vc[stagedFsTypeKey] = fsType
	ns.recordOnDiskFilesystem(ctx, volumeID, vc, fsType)
}

// priorFormat is what the volume is recorded as carrying, for the layer that
// has to decide whether a device reading blank is empty or merely unreadable.
//
// A node with no way to read the record contributes nothing rather than an
// empty answer, so that the layer can tell a driver that keeps no record from
// one whose record is empty.
func (ns *Server) priorFormat(volumeID string, vc map[string]string) priorFormatFunc {
	if ns.kubeClient == nil || ns.manager == nil {
		return nil
	}
	return func(ctx context.Context, _ plans.Volume) (string, error) {
		return ns.annotatedFilesystem(ctx, volumeID, vc)
	}
}

// volumeIsBeingDeleted reports whether this teardown is part of the volume
// going away for good.
//
// It is deliberately conservative, because the two ways to be wrong are not
// symmetrical. Answering no when the volume is being deleted leaves node-local
// objects behind for a host sweep to find. Answering yes when it is not would
// remove them from under a volume that is coming back, so nothing here treats
// the absence of a signal as one: a volume whose class says its storage is
// retained is never destroyed, whatever else is true of it.
func (ns *Server) volumeIsBeingDeleted(ctx context.Context, volumeID string) bool {
	if ns.kubeClient == nil || ns.manager == nil {
		return false
	}
	handle, err := csicommon.ParseVolumeHandle(volumeID)
	if err != nil {
		return false
	}

	pv, err := ns.manager.PersistentVolumeByLogicalVolumeID(ctx, handle.VolumeID)
	if err != nil {
		// A volume with no PersistentVolume left is one nothing will stage
		// again. Any other failure is a question that went unanswered, which is
		// not evidence of a deletion.
		return apierrors.IsNotFound(err)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		return false
	}
	if pv.DeletionTimestamp != nil ||
		pv.Status.Phase == corev1.VolumeReleased ||
		pv.Status.Phase == corev1.VolumeFailed {
		return true
	}
	if pv.Spec.ClaimRef == nil {
		return true
	}

	pvc, err := ns.manager.PersistentVolumeClaimByNamespaceAndName(
		ctx, pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		return false
	}
	return pvc.DeletionTimestamp != nil
}

// stagedFsType returns the filesystem a volume was staged with: the one
// recorded at stage time when it is there, and otherwise the one the volume
// capability asks for, which is all a volume staged by an older driver has.
func stagedFsType(volumeContext map[string]string, volCap *csi.VolumeCapability) string {
	if fsType := strings.TrimSpace(volumeContext[stagedFsTypeKey]); fsType != "" {
		return fsType
	}
	return fsTypeOrDefault(volCap)
}

// fsTypeOrDefault returns the requested filesystem type, defaulting to ext4.
func fsTypeOrDefault(volCap *csi.VolumeCapability) string {
	if fsType := volCap.GetMount().GetFsType(); fsType != "" {
		return fsType
	}
	return "ext4"
}
