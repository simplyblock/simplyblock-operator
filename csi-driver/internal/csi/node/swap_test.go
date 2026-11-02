// The node's swap service built over the node's own staging state: the same
// stack records the stage writes, and the claim of a swapped volume as the
// place a user sees what the node did on its own.

package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
	"github.com/simplyblock/atlas/volstack/swaprpc"
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
