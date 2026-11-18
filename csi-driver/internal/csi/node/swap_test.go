// The node's swap service built over the node's own staging state: the same
// stack records the stage writes, and the claim of a swapped volume as the
// place a user sees what the node did on its own.

package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
	"github.com/simplyblock/atlas/volstack/swaprpc"

	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

const swapUUID = "8e2dcb9d-1b2c-4f3a-9d4e-5f6a7b8c9d0e"

// U-38 (node half): the swapper reads the records the stage wrote, so a volume
// the node staged behind the indirection is one it can swap.
func TestTheNodesSwapperReadsTheStagingRecords(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	fabric, _ := json.Marshal(layers.FabricParams{NQN: "nqn:source", NSID: 1})
	dm, _ := json.Marshal(layers.DMLinearParams{Name: layers.DMLinearName(swapUUID)})
	if err := ns.stack.store.Write(volstack.Record{
		Version: volstack.RecordVersion, VolumeHandle: pvcTestHandle,
		Plan: []volstack.Entry{{Layer: "fabric", Params: fabric}, {Layer: "dmLinear", Params: dm}},
	}); err != nil {
		t.Fatal(err)
	}

	sw, err := ns.NewSwapper(SwapperConfig{NodeName: "worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	// Describe asks the kernel about the mapping, which a test host has no
	// device-mapper for, so the record's verdict is what is checked here.
	states, _ := sw.Describe(context.Background(), []string{"unstaged:handle"})
	if len(states) != 1 || states[0].Staged {
		t.Fatalf("an unstaged handle reads as %+v", states)
	}
	if _, err := sw.Suspend(context.Background(), []string{"unstaged:handle"}, farFuture()); err == nil ||
		!strings.Contains(err.Error(), "not staged behind") {
		t.Fatalf("Suspend of an unstaged volume = %v, want a refusal naming the indirection", err)
	}
}

// The watchdog's warning lands on the claim of the volume it resumed.
func TestASwapWarningLandsOnTheVolumesClaim(t *testing.T) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.simplyblock.io", VolumeHandle: pvcTestHandle},
			},
			ClaimRef: &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "apps", Name: "data", UID: "u1"},
		},
	}
	recorder := record.NewFakeRecorder(4)
	events := claimEvents{kube: fake.NewSimpleClientset(pv), recorder: recorder}

	events.Warning(pvcTestHandle, swaprpc.ReasonWatchdogFired, "resumed on its own")
	events.Warning("cluster:pool:unknown", swaprpc.ReasonWatchdogFired, "no claim to tell")

	select {
	case got := <-recorder.Events:
		if !strings.Contains(got, "Warning "+swaprpc.ReasonWatchdogFired) || !strings.Contains(got, "resumed on its own") {
			t.Fatalf("event = %q", got)
		}
	default:
		t.Fatal("no event was recorded for a volume with a claim")
	}
	select {
	case got := <-recorder.Events:
		t.Fatalf("an event was recorded for a volume with no claim: %q", got)
	default:
	}
}

func farFuture() time.Time { return time.Now().Add(time.Hour) }

// heldGate is a swap service that holds the named volumes.
type heldGate map[string]bool

func (g heldGate) Held(handle string) bool { return g[handle] }

// A volume whose mapping the swap service is holding is refused by every RPC
// that would act on that mapping, as Unavailable so kubelet retries after the
// window; a volume it does not hold goes through to the RPC's own checks.
func TestTheNodeRefusesToTouchAVolumeTheSwapServiceHolds(t *testing.T) {
	ns := &Server{volumeLocks: csicommon.NewVolumeLocks(), swapGate: heldGate{"held:handle": true}}
	staging := t.TempDir()
	rpcs := map[string]func() error{
		"NodeStageVolume": func() error {
			_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
				VolumeId: "held:handle", StagingTargetPath: staging})
			return err
		},
		"NodeUnstageVolume": func() error {
			_, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
				VolumeId: "held:handle", StagingTargetPath: staging})
			return err
		},
		"NodePublishVolume": func() error {
			_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
				VolumeId: "held:handle", StagingTargetPath: staging, TargetPath: staging + "/target"})
			return err
		},
		"NodeExpandVolume": func() error {
			_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
				VolumeId: "held:handle", StagingTargetPath: staging})
			return err
		},
	}
	for name, call := range rpcs {
		if code := status.Code(call()); code != codes.Unavailable {
			t.Errorf("%s of a held volume = %v, want Unavailable", name, code)
		}
	}

	// Not held: the RPC runs and fails on its own terms (no stashed context
	// here), which is anything but Unavailable.
	_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId: "free:handle", StagingTargetPath: staging})
	if code := status.Code(err); code == codes.Unavailable || code == codes.OK {
		t.Fatalf("NodeExpandVolume of a free volume = %v, want the RPC's own failure", code)
	}
}

// A node that has no swap service yet refuses nothing.
func TestTheGateIsOpenBeforeTheSwapServiceExists(t *testing.T) {
	ns := &Server{volumeLocks: csicommon.NewVolumeLocks()}
	if err := ns.refuseWhileHeld("any:handle"); err != nil {
		t.Fatalf("refuseWhileHeld without a service = %v, want nil", err)
	}
}

// Building the swap service is what closes the gate: from then on the node
// asks it, and the service takes the node's per-volume lock.
func TestNewSwapperInstallsItselfAsTheNodesGate(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	ns.volumeLocks = csicommon.NewVolumeLocks()
	sw, err := ns.NewSwapper(SwapperConfig{NodeName: "worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if ns.swapGate != swapGate(sw) {
		t.Fatal("the node does not ask the swapper it built")
	}
}

// The control plane holding the subsystem for a move is transient and comes
// back Unavailable; any other planning failure stays Internal.
func TestAConnectHeldByAMoveIsUnavailableNotInternal(t *testing.T) {
	held := fmt.Errorf("volume c:p:v: %w: nqn.src is moving", controlplane.ErrVolumeHeldByMove)
	if code := status.Code(stackError(held, "plan %s", "c:p:v")); code != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", code)
	}
	if code := status.Code(stackError(errors.New("no device"), "plan %s", "c:p:v")); code != codes.Internal {
		t.Fatalf("code = %v, want Internal", code)
	}
	if msg := stackError(held, "plan %s", "c:p:v").Error(); !strings.Contains(msg, "nqn.src is moving") {
		t.Fatalf("message %q lost the control plane's reason", msg)
	}
}
