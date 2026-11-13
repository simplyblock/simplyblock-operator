// The node's half of a striped volume's crash-consistent snapshot: suspending
// the stripe's device-mapper device while the controller takes the member
// snapshots, and resuming it (csi-driver/docs/striped-volumes.md).
//
// The controller reaches this through the operator's relay
// (atlas-lib/freeze/freezerpc). What is here is which device a volume handle
// means on this node, and that the freeze holds the volume's lock: nothing
// stages, unstages, publishes, or grows a volume while its device is suspended,
// since each of those would wait on the suspended device or change what is
// underneath the snapshot.

package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/devmapper"
	"github.com/simplyblock/atlas/freeze"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/plans"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// freezeRecordDir is where a freeze is recorded while it lasts, inside the
// stack-record mount so that it survives a plugin restart: the restarted
// plugin resumes what its predecessor left suspended.
const freezeRecordDir = stackRecordDir + "/freezes"

// newFreezes is the node's freeze holds over the host's device-mapper.
func newFreezes() *freeze.Holds {
	return freeze.New(freeze.Config{
		Mapper: devmapper.New(nil),
		Dir:    freezeRecordDir,
	})
}

// RecoverFreezes resumes every device a previous run of the plugin left
// suspended. It runs once at start, before the plugin serves anything.
func (ns *Server) RecoverFreezes(ctx context.Context) {
	if ns.freezes == nil {
		return
	}
	if err := ns.freezes.Recover(ctx); err != nil {
		klog.Errorf("could not resume every device a previous run left frozen: %v", err)
	}
}

// Freeze suspends the striped device of the volume handle names, for at most
// deadline. It reports false for a volume this node does not stage, which has
// no writer here and nothing to suspend.
func (ns *Server) Freeze(ctx context.Context, handle, token string, deadline time.Duration) (bool, error) {
	device, staged, err := ns.stripedDevice(handle)
	if err != nil || !staged {
		return false, err
	}
	acquire := func(ctx context.Context) (func(), error) { return lockWithin(ctx, ns.volumeLocks, handle) }
	start := time.Now()
	err = ns.freezes.Freeze(ctx, handle, device, token, deadline, acquire)
	switch {
	case errors.Is(err, freeze.ErrHeld):
		return false, status.Error(codes.Aborted, err.Error())
	case err != nil:
		return false, status.Errorf(codes.Unavailable, "freeze %s: %v", handle, err)
	}
	klog.Infof("froze striped volume %s (%s) in %v, for at most %v", handle, device, time.Since(start), deadline)
	return true, nil
}

// Thaw resumes the volume's device and reports whether it stayed suspended
// from Freeze until now.
func (ns *Server) Thaw(ctx context.Context, handle, token string) (bool, error) {
	held, err := ns.freezes.Thaw(ctx, handle, token)
	if errors.Is(err, freeze.ErrTokenMismatch) {
		return false, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		klog.Errorf("thaw of striped volume %s: %v", handle, err)
	}
	klog.Infof("thawed striped volume %s: the freeze held: %v", handle, held)
	return held, err
}

// stripedDevice is the device-mapper name of the striped logical volume handle
// names on this node, and whether the volume is staged here at all.
//
// The stack record says so: it is written before a stage's first side effect
// and removed after an unstage's last, and it holds the layers a striped stage
// built. The logical volume is named after the first member, the handle's own
// lvol, the way the plan named it.
func (ns *Server) stripedDevice(handle string) (string, bool, error) {
	record, err := ns.stack.store.Load(handle)
	if errors.Is(err, volstack.ErrNoRecord) {
		return "", false, nil
	}
	if err != nil {
		return "", false, status.Errorf(codes.Internal, "read the stack record of %s: %v", handle, err)
	}
	layers := recordedLayers(record)
	if !slices.Contains(layers, layerMembers) || !slices.Contains(layers, layerLVMLogicalVolume) {
		return "", false, status.Errorf(codes.FailedPrecondition,
			"volume %s is staged here as %v, which is not a striped volume", handle, layers)
	}
	h, err := csicommon.ParseVolumeHandle(handle)
	if err != nil {
		return "", false, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	lv := lvm.LogicalVolume{
		VolumeGroup: lvm.VolumeGroup{Name: plans.VolumeGroupName(h.VolumeID)},
		Name:        plans.LogicalVolumeName(h.VolumeID),
	}
	return lvm.DeviceMapperName(lv), true, nil
}

// lockWithin takes the volume's lock, or gives up when ctx ends. A lock taken
// after that is released at once, so a late acquisition holds nothing.
func lockWithin(ctx context.Context, locks *csicommon.VolumeLocks, id string) (func(), error) {
	got := make(chan func(), 1)
	go func() { got <- locks.Lock(id) }()
	select {
	case unlock := <-got:
		return unlock, nil
	case <-ctx.Done():
		go func() { (<-got)() }()
		return nil, fmt.Errorf("volume %s is busy with another operation: %w", id, ctx.Err())
	}
}
