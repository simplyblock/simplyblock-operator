// The arbitration reconciler's host-fencing duties: settings, host NQNs, the
// StorageFenced condition, the release once the node is clean, and not taking
// its own remediation's taint for fencing evidence
// (sbcli docs/design/two-node-kubernetes-fencing.md §3.4).

package twonode

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

const kernelNQNB = "nqn.2014-08.org.nvmexpress:uuid:kernel-b"

// fenceSince is when the test's host fences were armed: one hour before the
// reconciler's fixed clock.
var fenceSince = time.Date(2026, 10, 9, 11, 0, 0, 0, time.UTC)

// hostFencingSetup is setup with spec.arbitration.hostFencing on, node UIDs,
// and worker-b's kernel host NQN published by the CSI node plugin.
func hostFencingSetup(t *testing.T, api *fakeAPI) (*ArbitrationReconciler, client.Client) {
	t.Helper()
	r, c := setup(t, api, nil)
	ctx := context.Background()
	var cl simplyblockv1alpha2.StorageCluster
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "edge"}, &cl); err != nil {
		t.Fatal(err)
	}
	cl.Spec.Arbitration = &simplyblockv1alpha2.ArbitrationSpec{HostFencing: ptr.To(true)}
	if err := c.Update(ctx, &cl); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{workerA, workerB} {
		var n corev1.Node
		if err := c.Get(ctx, client.ObjectKey{Name: w}, &n); err != nil {
			t.Fatal(err)
		}
		n.UID = types.UID("uid-" + w)
		if w == workerB {
			n.Annotations = map[string]string{kube.AnnoNVMeHostNQN: kernelNQNB}
		}
		if err := c.Update(ctx, &n); err != nil {
			t.Fatal(err)
		}
	}
	return r, c
}

func fenceRecord(state string) *Record {
	return &Record{State: "degraded", Epoch: 7, Enabled: true, HostFencing: true,
		HostFences: map[string]HostFence{"id-b": {State: state, On: "id-a", Epoch: 7, Since: fenceSince}}}
}

func storageFenced(t *testing.T, c client.Client, worker string) *corev1.NodeCondition {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: worker}, &n); err != nil {
		t.Fatal(err)
	}
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == simplyblockv1alpha2.ConditionStorageFenced {
			return &n.Status.Conditions[i]
		}
	}
	return nil
}

func TestHostFencingSettingsArePushedToTheControlPlane(t *testing.T) {
	api := &fakeAPI{rec: &Record{State: "steady", Enabled: false}}
	r, _ := hostFencingSetup(t, api)
	reconcile(t, r)
	if !slices.Equal(api.settings, []string{"true,true"}) {
		t.Fatalf("settings %v", api.settings)
	}
	api.settings, api.rec = nil, &Record{State: "steady", Enabled: true, HostFencing: true}
	reconcile(t, r)
	if len(api.settings) != 0 {
		t.Fatalf("matching settings were written again: %v", api.settings)
	}
}

// Both identities: plain volumes connect with the kernel default, access-
// controlled ones with the UID form. Revoking one would leave the other in.
func TestBothHostIdentitiesAreReported(t *testing.T) {
	api := &fakeAPI{rec: &Record{State: "steady", Enabled: true, HostFencing: true}}
	r, c := hostFencingSetup(t, api)
	reconcile(t, r)
	want := []string{kernelNQNB, nqn.Host("uid-" + workerB)}
	slices.Sort(want)
	if got := api.clientHosts["id-b"]; !slices.Equal(got, want) {
		t.Fatalf("client hosts of id-b = %v, want %v", got, want)
	}
	if got := api.clientHosts["id-a"]; !slices.Equal(got, []string{nqn.Host("uid-" + workerA)}) {
		t.Fatalf("client hosts of id-a = %v", got)
	}
	var sn simplyblockv1alpha2.StorageNode
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "sn-b"}, &sn)
	if sn.Status.Remediation == nil || !slices.Equal(sn.Status.Remediation.ReportedHostNQNs, want) {
		t.Fatalf("reported host NQNs %+v", sn.Status.Remediation)
	}
}

func TestRevokedFenceSetsStorageFencedOnTheLoserOnly(t *testing.T) {
	api := &fakeAPI{rec: fenceRecord(HostFenceRevoked)}
	r, c := hostFencingSetup(t, api)
	if res := reconcile(t, r); res.RequeueAfter != requeueActive {
		t.Fatalf("requeue %v", res.RequeueAfter)
	}
	cond := storageFenced(t, c, workerB)
	if cond == nil || cond.Status != corev1.ConditionTrue || cond.Reason != simplyblockv1alpha2.StorageFencedReasonWitness ||
		!strings.Contains(cond.Message, workerA) {
		t.Fatalf("worker-b condition %+v", cond)
	}
	if storageFenced(t, c, workerA) != nil {
		t.Fatal("the winner must not be storage-fenced")
	}
	var cl simplyblockv1alpha2.StorageCluster
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "edge"}, &cl)
	hf := cl.Status.Arbitration.HostFences
	if len(hf) != 1 || hf[0].Node != workerB || hf[0].Winner != workerA || hf[0].State != HostFenceRevoked {
		t.Fatalf("status host fences %+v", hf)
	}

	// The fence goes: the condition turns False.
	api.rec = &Record{State: "steady", Enabled: true, HostFencing: true}
	reconcile(t, r)
	if cond := storageFenced(t, c, workerB); cond == nil || cond.Status != corev1.ConditionFalse {
		t.Fatalf("worker-b condition after the release %+v", cond)
	}
}

// A fence still revoking is not a fence yet: the loser may still reach the
// winner, so Kubernetes must not start a second instance of its workloads.
func TestRevokingFenceSetsNoCondition(t *testing.T) {
	r, c := hostFencingSetup(t, &fakeAPI{rec: fenceRecord("revoking")})
	reconcile(t, r)
	if storageFenced(t, c, workerB) != nil {
		t.Fatal("a revoking fence must not set StorageFenced")
	}
}

func TestNoConditionWithoutHostFencing(t *testing.T) {
	r, c := setup(t, &fakeAPI{rec: fenceRecord(HostFenceRevoked)}, nil)
	reconcile(t, r)
	if storageFenced(t, c, workerB) != nil {
		t.Fatal("a cluster without hostFencing must get no StorageFenced condition")
	}
}

func addPod(t *testing.T, c client.Client, name, worker string, created time.Time, tolerateOOS bool) {
	t.Helper()
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps", CreationTimestamp: metav1.NewTime(created)},
		Spec: corev1.PodSpec{NodeName: worker, Containers: []corev1.Container{{Name: "c", Image: "i"}}}}
	if tolerateOOS {
		p.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}}
	}
	if err := c.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

// A pod from before the fence may be the first instance of a workload running
// on the peer now; the host gets its access back only once it is gone.
func TestReleaseWaitsUntilNoPodFromBeforeTheFenceIsLeft(t *testing.T) {
	api := &fakeAPI{rec: fenceRecord(HostFenceReleasable)}
	r, c := hostFencingSetup(t, api)
	addPod(t, c, "vm-old", workerB, fenceSince.Add(-time.Hour), false)
	reconcile(t, r)
	if len(api.releases) != 0 {
		t.Fatalf("released with a pod from before the fence on the node: %v", api.releases)
	}
	if cond := storageFenced(t, c, workerB); cond == nil || cond.Status != corev1.ConditionTrue {
		t.Fatalf("a releasable fence keeps the node StorageFenced: %+v", cond)
	}
	var p corev1.Pod
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "apps", Name: "vm-old"}, &p)
	if err := c.Delete(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if !slices.Equal(api.releases, []string{"id-b"}) {
		t.Fatalf("releases %v", api.releases)
	}
}

// The storage pods tolerate the out-of-service taint and stay: the heal needs
// them. Pods started after the fence are not its first instances either.
func TestReleaseIgnoresStoragePodsAndNewPods(t *testing.T) {
	api := &fakeAPI{rec: fenceRecord(HostFenceReleasable)}
	r, c := hostFencingSetup(t, api)
	addPod(t, c, "spdk", workerB, fenceSince.Add(-time.Hour), true)
	addPod(t, c, "new", workerB, fenceSince.Add(time.Minute), false)
	addPod(t, c, "elsewhere", workerA, fenceSince.Add(-time.Hour), false)
	reconcile(t, r)
	if !slices.Equal(api.releases, []string{"id-b"}) {
		t.Fatalf("releases %v", api.releases)
	}
}

func TestReleaseWaitsForAReadyNode(t *testing.T) {
	api := &fakeAPI{rec: fenceRecord(HostFenceReleasable)}
	r, c := hostFencingSetup(t, api)
	var n corev1.Node
	_ = c.Get(context.Background(), client.ObjectKey{Name: workerB}, &n)
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}}
	if err := c.Status().Update(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if len(api.releases) != 0 {
		t.Fatalf("released a node that is not Ready: %v", api.releases)
	}
}

// Reported as evidence, the remediation's own taint would make the arbiter
// treat the healed node as gone and fence it again.
func TestOwnOutOfServiceTaintIsNoFencingEvidence(t *testing.T) {
	oos := &corev1.Taint{Key: simplyblockv1alpha2.TaintOutOfService, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute}
	api := &fakeAPI{rec: &Record{State: "steady", Enabled: true, HostFencing: true}}
	r, c := setup(t, api, oos)
	var n corev1.Node
	_ = c.Get(context.Background(), client.ObjectKey{Name: workerB}, &n)
	n.Annotations = map[string]string{simplyblockv1alpha2.AnnoOutOfServiceBy: simplyblockv1alpha2.OutOfServiceByStorageFence}
	if err := c.Update(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if !testsupport.Contains(api.reports, "id-b=false") || testsupport.Contains(api.reports, "id-b=true") {
		t.Fatalf("reports %v", api.reports)
	}
}

func TestArbitrationOffClearsStorageFenced(t *testing.T) {
	api := &fakeAPI{rec: fenceRecord(HostFenceRevoked)}
	r, c := hostFencingSetup(t, api)
	reconcile(t, r)
	var cl simplyblockv1alpha2.StorageCluster
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "edge"}, &cl)
	cl.Spec.EnableArbitration = ptr.To(false)
	if err := c.Update(context.Background(), &cl); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if cond := storageFenced(t, c, workerB); cond == nil || cond.Status != corev1.ConditionFalse {
		t.Fatalf("condition after arbitration off %+v", cond)
	}
	if !slices.Contains(api.settings, "false,false") {
		t.Fatalf("settings %v", api.settings)
	}
}
