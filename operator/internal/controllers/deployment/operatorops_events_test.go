// What a refusal has to fit into, which is an event.
//
// The Kubernetes event API caps a message at 1024 characters and rejects a
// longer one outright, so a refusal whose length grows with the fleet is a
// refusal that stops being delivered on exactly the fleets where it matters
// most. These cases pin the bound and the accounting it has to keep.

package deployment

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	"github.com/simplyblock/atlas/blockdev"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// declinedFleet builds a run over workers whose every device is refused, in the
// shape a re-install finds: two disks carrying data and one holding the root
// filesystem, on each worker.
func declinedFleet(t *testing.T, workers int) discoveryCase {
	t.Helper()

	ops := &simplyblockv1alpha2.OperatorOps{
		ObjectMeta: metav1.ObjectMeta{Name: opsName, Namespace: opsNamespace},
		Spec:       simplyblockv1alpha2.OperatorOpsSpec{Action: simplyblockv1alpha2.OperatorOpsActionDiscover},
	}

	var objects []client.Object
	for i := 1; i <= workers; i++ {
		node := fmt.Sprintf("worker-%02d", i)
		ops.Status.Workers = append(ops.Status.Workers, node)
		objects = append(objects, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: node},
			Status: corev1.NodeStatus{
				Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: fmt.Sprintf("10.10.10.%d", i)}},
			},
		})
		objects = append(objects, declinedReport(t, node))
	}
	return discoveryCase{Ops: ops, Objects: objects}
}

// declinedReport is one worker's probe result with nothing usable on it.
func declinedReport(t *testing.T, node string) *corev1.ConfigMap {
	t.Helper()

	const tb = uint64(1) << 40
	report := nodeprobe.Report{
		Version: nodeprobe.ReportVersion,
		Node:    node,
		CPU: nodeprobe.CPU{
			OnlineCPUs: 32, PhysicalCores: 16, Sockets: 2, ThreadsPerCore: 2, HyperThreading: true,
			NUMANodes: []nodeprobe.NUMACPUs{{Node: 0, OnlineCPUs: []int{0, 1, 2, 3}, PhysicalCores: 16}},
		},
		HugePages: []nodeprobe.HugePagePool{{
			SizeBytes: 1 << 30, Total: 32, Free: 32,
			NUMANodes: []nodeprobe.NUMAHugePages{{Node: 0, Total: 32, Free: 32}},
		}},
	}
	for i := range 2 {
		report.Devices = append(report.Devices, nodeprobe.Device{
			Name: fmt.Sprintf("nvme%dn1", i), Path: fmt.Sprintf("/dev/nvme%dn1", i),
			PCIAddress: fmt.Sprintf("0000:0%d:00.0", i), SizeBytes: 2 * tb,
			Kind: string(blockdev.KindDisk), Transport: string(blockdev.TransportNVMe),
			Available: false, Content: "Foreign",
			Rejections: []nodeprobe.Rejection{{
				Reason: string(blockdev.ReasonNotBlank),
				Detail: "no known signature, and the probed regions are not empty: first non-zero byte at 0",
			}},
		})
	}
	report.Devices = append(report.Devices, nodeprobe.Device{
		Name: "nvme2n1", Path: "/dev/nvme2n1", PCIAddress: "0000:02:00.0", SizeBytes: tb,
		Kind: string(blockdev.KindDisk), Transport: string(blockdev.TransportNVMe),
		Available: false,
		Rejections: []nodeprobe.Rejection{
			{Reason: string(blockdev.ReasonMounted), Detail: "mounted at [/boot /sysroot /var]"},
			{Reason: string(blockdev.ReasonBusy), Detail: "the kernel refused an exclusive open, and gives no reason"},
			{Reason: string(blockdev.ReasonPartitioned), Detail: "the device carries the partitions [nvme2n1p1 nvme2n1p2]"},
		},
	})

	cm, err := nodeprobe.ConfigMap(opsNamespace, opsName, nil, report)
	if err != nil {
		t.Fatalf("render the report ConfigMap: %v", err)
	}
	return cm
}

// TestTheRefusalFitsAnEventWhateverTheFleetSize covers the message a run writes
// when no worker has a usable device.
//
// Regression: 2026-09-20-discovery-refusal-too-long-for-an-event — the message
// carried one clause per worker, so at six workers it reached 1193 characters
// and the API server refused the event with `is invalid: message: Invalid
// value: "": can have at most 1024 characters`, twice, and the recorder does
// not retry. `kubectl describe operatorops` then showed a run that had failed
// for no stated reason, and the only surviving copy of why was the operator's
// own log. It grows with the fleet, so the larger the cluster the more certain
// the loss.
func TestTheRefusalFitsAnEventWhateverTheFleetSize(t *testing.T) {
	for _, workers := range []int{6, 32} {
		t.Run(fmt.Sprintf("%d-workers", workers), func(t *testing.T) {
			got := runDiscoveryCase(t, declinedFleet(t, workers))

			if got.Failure == "" {
				t.Fatal("a fleet with nothing usable produced no refusal")
			}
			if len(got.Failure) > maxEventMessage {
				t.Errorf("the refusal is %d characters and an event carries %d, so it is dropped:\n%s",
					len(got.Failure), maxEventMessage, got.Failure)
			}
		})
	}
}

// The bound is not an excuse to stop saying what happened: the counts are what
// distinguishes a fleet with no disks from one whose disks are all held.
func TestTheRefusalStillAccountsForEveryDevice(t *testing.T) {
	const workers = 6
	got := runDiscoveryCase(t, declinedFleet(t, workers))

	for _, want := range []string{"18", string(blockdev.ReasonNotBlank), string(blockdev.ReasonMounted)} {
		if !strings.Contains(got.Failure, want) {
			t.Errorf("the refusal does not mention %q, so a reader cannot tell what was found:\n%s",
				want, got.Failure)
		}
	}
}

// TestALongEventIsDeliveredRatherThanDropped covers every other message this
// reconciler emits, since any of them may be built from a list.
func TestALongEventIsDeliveredRatherThanDropped(t *testing.T) {
	recorder := events.NewFakeRecorder(4)
	r := &OperatorOpsReconciler{Recorder: recorder}
	ops := &simplyblockv1alpha2.OperatorOps{
		ObjectMeta: metav1.ObjectMeta{Name: opsName, Namespace: opsNamespace},
	}

	r.event(ops, corev1.EventTypeWarning, OperationFailed, strings.Repeat("x", 4000))

	select {
	case line := <-recorder.Events:
		if got := len(eventMessage(line)); got > maxEventMessage {
			t.Errorf("the event carries %d characters and the API server takes %d", got, maxEventMessage)
		}
	default:
		t.Fatal("no event was recorded")
	}
}
