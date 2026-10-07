package controller

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/link"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const restartedPodUID = "mds-uid-2"

// readyPodHosted is a Ready export assembled by the pod instance testMDSPodUID.
func readyPodHosted() *simplyblockv1alpha2.NFSExport {
	return testExport(func(e *simplyblockv1alpha2.NFSExport) {
		podHosted(simplyblockv1alpha2.NFSExportPhaseReady)(e)
		e.Status.ObservedGeneration = e.Generation
	})
}

func runningMDSPod(uid, ip string) *corev1.Pod {
	p := mdsPod()
	p.UID = types.UID(uid)
	p.Status.PodIP = ip
	return p
}

func newResyncReconciler(
	t *testing.T, asm *fakeAssembler, objects ...client.Object,
) (*NFSExportReconciler, client.Client, *reasonRecorder) {
	t.Helper()
	r, cl := newExportReconciler(t, asm, objects...)
	recorder := &reasonRecorder{}
	r.Recorder = recorder
	return r, cl, recorder
}

// A restarted pod has a guest that lost every mount and exports entry. The
// mismatch between the pod that assembled the export and the pod running now
// is what says so, without asking the guest.
func TestAReadyExportIsReassembledAfterItsPodRestarted(t *testing.T) {
	asm := &fakeAssembler{}
	r, cl, events := newResyncReconciler(t, asm,
		readyPodHosted(), runningMDSPod(restartedPodUID, testMDSPodIP))

	reconcileExport(t, r)

	if want := link.MDSPeer(testMDSPod); len(asm.created) != 1 || asm.created[0] != want {
		t.Errorf("CreateExport reached %v, want [%s]", asm.created, want)
	}
	got := loadExport(t, cl)
	if got.Status.AssembledBy != restartedPodUID {
		t.Errorf("assembledBy = %q, want the running pod %q", got.Status.AssembledBy, restartedPodUID)
	}
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseReady {
		t.Errorf("phase = %q, want Ready: a resync is not a new assembly", got.Status.Phase)
	}
	if !slices.Contains(events.reasons, "MDSResynced") {
		t.Errorf("events = %v, want MDSResynced", events.reasons)
	}
}

// The pod IP changes with a restart. The EndpointSlice is repointed before the
// export is assembled again, so a client retrying reaches the new guest.
func TestAResyncRepointsTheEndpointSliceAtTheNewPodIP(t *testing.T) {
	const newIP = "10.244.7.4"
	asm := &fakeAssembler{}
	r, cl, events := newResyncReconciler(t, asm,
		readyPodHosted(), runningMDSPod(restartedPodUID, newIP))

	reconcileExport(t, r)

	var eps discoveryv1.EndpointSlice
	key := client.ObjectKey{Name: utils.NFSExportEndpointSliceName(testExportName), Namespace: testExportNS}
	if err := cl.Get(context.Background(), key, &eps); err != nil {
		t.Fatalf("reading the EndpointSlice: %v", err)
	}
	if len(eps.Endpoints) != 1 || eps.Endpoints[0].Addresses[0] != newIP {
		t.Errorf("EndpointSlice endpoints = %+v, want the new pod IP %s", eps.Endpoints, newIP)
	}
	if got := loadExport(t, cl); got.Status.MDSNodeIP != newIP {
		t.Errorf("mdsNodeIP = %q, want %q", got.Status.MDSNodeIP, newIP)
	}
	if !slices.Contains(events.reasons, "MDSAddressChanged") {
		t.Errorf("events = %v, want MDSAddressChanged", events.reasons)
	}
}

// The pod that assembled it is still running: a health check, and nothing else.
func TestAReadyExportOnItsAssemblingPodIsOnlyChecked(t *testing.T) {
	asm := &fakeAssembler{}
	r, _, _ := newResyncReconciler(t, asm,
		readyPodHosted(), runningMDSPod(testMDSPodUID, testMDSPodIP))

	reconcileExport(t, r)

	if len(asm.created) != 0 || len(asm.checked) != 1 {
		t.Errorf("created %v, checked %v, want only a check", asm.created, asm.checked)
	}
}

// A restarted pod that has not linked yet is waited for, and the export is not
// marked resynced before it was.
func TestAResyncWaitsForTheRestartedPodsSession(t *testing.T) {
	asm := &fakeAssembler{noSessions: true}
	r, cl, _ := newResyncReconciler(t, asm,
		readyPodHosted(), runningMDSPod(restartedPodUID, testMDSPodIP))

	res := reconcileExport(t, r)

	if res.RequeueAfter != nfsExportNoSessionRequeue {
		t.Errorf("requeue = %v, want %v", res.RequeueAfter, nfsExportNoSessionRequeue)
	}
	if got := loadExport(t, cl); got.Status.AssembledBy != testMDSPodUID {
		t.Errorf("assembledBy = %q, want it unchanged until the export is reassembled", got.Status.AssembledBy)
	}
}

// The first assembly records the pod instance that did it, which is what a
// later restart is detected against.
func TestAssemblyRecordsTheAssemblingPod(t *testing.T) {
	asm := &fakeAssembler{}
	export := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		podHosted(simplyblockv1alpha2.NFSExportPhaseAssembling)(e)
		e.Status.AssembledBy = ""
	})
	r, cl, _ := newResyncReconciler(t, asm, export, runningMDSPod(restartedPodUID, testMDSPodIP))

	reconcileExport(t, r)

	if got := loadExport(t, cl); got.Status.AssembledBy != restartedPodUID {
		t.Errorf("assembledBy = %q, want %q", got.Status.AssembledBy, restartedPodUID)
	}
}

// A change to a metadata server pod wakes the exports bound to it, and no
// others.
func TestAnMDSPodEventEnqueuesTheExportsBoundToIt(t *testing.T) {
	bound := readyPodHosted()
	other := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Name = "nfsexp-other"
		e.Status.MDSPodName = "simplyblock-pnfs-mds-7c3e9a10-0"
	})
	nodeHosted := testExport(func(e *simplyblockv1alpha2.NFSExport) {
		e.Name = "nfsexp-node"
		e.Status.MDSNodeName = testMDSHost
	})
	r, _, _ := newResyncReconciler(t, &fakeAssembler{}, bound, other, nodeHosted)

	requests := r.exportsBoundToPod(context.Background(), mdsPod())

	if len(requests) != 1 || requests[0].Name != testExportName || requests[0].Namespace != testExportNS {
		t.Errorf("requests = %v, want only %s/%s", requests, testExportNS, testExportName)
	}
	if got := r.exportsBoundToPod(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "default"},
	}); len(got) != 0 {
		t.Errorf("an unrelated pod enqueued %v", got)
	}
}
