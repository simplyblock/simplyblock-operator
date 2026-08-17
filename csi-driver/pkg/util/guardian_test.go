// White-box tests for coordinatedSubsystemRestart and its gate logic.
package util

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/simplyblock/atlas/errs"
	atlasnvme "github.com/simplyblock/atlas/nvme"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// nullDeviceResolver satisfies atlasnvme.DeviceResolver for unit tests.
// All lookups return ErrNotFound so subsystemNQN falls back to the cached NQN
// stored in LvolState.SubsystemNQN (empty in most tests → individual restart path).
type nullDeviceResolver struct{}

func (nullDeviceResolver) List(_ context.Context) ([]atlasnvme.Device, error) {
	return nil, nil
}
func (nullDeviceResolver) ListWithSelector(_ context.Context, _ atlasnvme.DeviceSelector) ([]atlasnvme.Device, error) {
	return nil, nil
}
func (nullDeviceResolver) ByUUID(_ context.Context, _ string) (atlasnvme.Device, error) {
	return atlasnvme.Device{}, errs.ErrNotFound
}
func (nullDeviceResolver) ByDevicePath(_ context.Context, _ string) (atlasnvme.Device, error) {
	return atlasnvme.Device{}, errs.ErrNotFound
}
func (nullDeviceResolver) ByNamespace(_ context.Context, _ string, _ atlasnvme.NamespaceID) (atlasnvme.Device, error) {
	return atlasnvme.Device{}, errs.ErrNotFound
}

const (
	testOptInKey   = "simplyblock.io/auto-restart-on-pathloss"
	testOptInValue = "true"
	testNS         = "test-ns"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func boolPtr(b bool) *bool { return &b }

func newTestGuardian(cs *fake.Clientset) *Guardian {
	return &Guardian{
		cfg: GuardianConfig{
			OptInLabelKey:   testOptInKey,
			OptInLabelValue: testOptInValue,
			RestartBackoff:  10 * time.Minute,
			GraceSeconds:    0,
			StatePath:       "", // disable on-disk persistence
		},
		cs:                 cs,
		devices:            nullDeviceResolver{},
		lastRestart:        make(map[string]time.Time),
		lvols:              make(map[string]*LvolState),
		clusterWasInactive: make(map[string]bool),
	}
}

// controlledOptInPod returns a pod that is controller-managed and carries the
// auto-restart opt-in label. It has no PVC volumes so the slow-path opt-in
// check (PVC→PV→StorageClass) is never reached and g.manager can be nil.
func controlledOptInPod(name, uid string) v1.Pod {
	return v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
			UID:       types.UID(uid),
			Labels:    map[string]string{testOptInKey: testOptInValue},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "rs-" + name, Controller: boolPtr(true)},
			},
		},
	}
}

// controlledNonOptInPod is controller-managed but carries no opt-in label and
// no PVC volumes so podUsesOptedInSimplyBlockStorageClass returns (false, nil).
func controlledNonOptInPod(name, uid string) v1.Pod {
	return v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
			UID:       types.UID(uid),
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "rs-" + name, Controller: boolPtr(true)},
			},
		},
	}
}

// standaloneOptInPod carries the opt-in label but has no OwnerReference so
// controllerManaged returns false.
func standaloneOptInPod(name, uid string) v1.Pod {
	return v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
			UID:       types.UID(uid),
			Labels:    map[string]string{testOptInKey: testOptInValue},
		},
	}
}

func buildUIDToPod(pods ...v1.Pod) map[string]v1.Pod {
	m := make(map[string]v1.Pod, len(pods))
	for _, p := range pods {
		m[string(p.UID)] = p
	}
	return m
}

// toRuntimeObjs converts pods to the runtime.Object slice that
// fake.NewSimpleClientset expects so they are pre-registered in its tracker.
func toRuntimeObjs(pods ...v1.Pod) []runtime.Object {
	objs := make([]runtime.Object, len(pods))
	for i := range pods {
		p := pods[i]
		objs[i] = &p
	}
	return objs
}

// ─── tests ────────────────────────────────────────────────────────────────────

// No lvolPods entries → zero candidates → returns 0.
func TestCoordinatedSubsystemRestart_NoCandidates_EmptyLvolPods(t *testing.T) {
	g := newTestGuardian(fake.NewSimpleClientset())
	got := g.coordinatedSubsystemRestart(
		context.Background(), "cid",
		[]string{"lvol-1"},
		map[string][]string{},
		map[string]v1.Pod{},
	)
	if got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
}

// UID appears in lvolPods but not in uidToPod → pod is skipped → zero candidates.
func TestCoordinatedSubsystemRestart_NoCandidates_UnknownUID(t *testing.T) {
	g := newTestGuardian(fake.NewSimpleClientset())
	got := g.coordinatedSubsystemRestart(
		context.Background(), "cid",
		[]string{"lvol-1"},
		map[string][]string{"lvol-1": {"uid-missing"}},
		map[string]v1.Pod{},
	)
	if got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
}

// A standalone pod (no OwnerReference) fails the controllerManaged gate and
// suppresses the entire group even though it is opted in.
func TestCoordinatedSubsystemRestart_Gate_StandalonePodSuppressesGroup(t *testing.T) {
	pods := []v1.Pod{
		controlledOptInPod("pod-a", "uid-1"),
		standaloneOptInPod("pod-b", "uid-2"), // no controller → gate fires
	}
	g := newTestGuardian(fake.NewSimpleClientset(toRuntimeObjs(pods...)...))
	lvolPods := map[string][]string{"lvol-1": {"uid-1", "uid-2"}}
	uidToPod := buildUIDToPod(pods...)

	got := g.coordinatedSubsystemRestart(context.Background(), "cid", []string{"lvol-1"}, lvolPods, uidToPod)
	if got != 0 {
		t.Fatalf("standalone pod should suppress group; got %d deletions", got)
	}
	// pod-a must not have been deleted
	if _, err := g.cs.CoreV1().Pods(testNS).Get(context.Background(), "pod-a", metav1.GetOptions{}); err != nil {
		t.Errorf("pod-a should not have been deleted: %v", err)
	}
}

// A non-opted-in pod fails the podOptedInForAutoRestart gate and suppresses
// the entire group — this is the all-or-nothing safety guarantee.
func TestCoordinatedSubsystemRestart_Gate_NonOptedInSuppressesGroup(t *testing.T) {
	pods := []v1.Pod{
		controlledOptInPod("pod-a", "uid-1"),
		controlledNonOptInPod("pod-b", "uid-2"), // no opt-in label → gate fires
	}
	g := newTestGuardian(fake.NewSimpleClientset(toRuntimeObjs(pods...)...))
	lvolPods := map[string][]string{"lvol-1": {"uid-1", "uid-2"}}
	uidToPod := buildUIDToPod(pods...)

	got := g.coordinatedSubsystemRestart(context.Background(), "cid", []string{"lvol-1"}, lvolPods, uidToPod)
	if got != 0 {
		t.Fatalf("non-opted-in pod should suppress group; got %d deletions", got)
	}
	if _, err := g.cs.CoreV1().Pods(testNS).Get(context.Background(), "pod-a", metav1.GetOptions{}); err != nil {
		t.Errorf("pod-a should not have been deleted: %v", err)
	}
}

// A pod whose lastRestart is within RestartBackoff fires the backoff gate,
// suppressing the whole group.
func TestCoordinatedSubsystemRestart_Gate_BackoffSuppressesGroup(t *testing.T) {
	pod := controlledOptInPod("pod-a", "uid-1")
	g := newTestGuardian(fake.NewSimpleClientset(&pod))
	g.lastRestart["uid-1"] = time.Now() // recently restarted

	got := g.coordinatedSubsystemRestart(
		context.Background(), "cid",
		[]string{"lvol-1"},
		map[string][]string{"lvol-1": {"uid-1"}},
		buildUIDToPod(pod),
	)
	if got != 0 {
		t.Fatalf("pod in backoff should suppress group; got %d deletions", got)
	}
}

// When lastRestart is older than RestartBackoff the backoff is expired and
// the restart proceeds normally.
func TestCoordinatedSubsystemRestart_Gate_ExpiredBackoffAllowsRestart(t *testing.T) {
	pod := controlledOptInPod("pod-a", "uid-1")
	g := newTestGuardian(fake.NewSimpleClientset(&pod))
	g.lastRestart["uid-1"] = time.Now().Add(-15 * time.Minute) // older than 10 min backoff

	got := g.coordinatedSubsystemRestart(
		context.Background(), "cid",
		[]string{"lvol-1"},
		map[string][]string{"lvol-1": {"uid-1"}},
		buildUIDToPod(pod),
	)
	if got != 1 {
		t.Fatalf("expired backoff should allow restart; expected 1, got %d", got)
	}
}

// Happy path: all candidates pass every gate — all pods are deleted and
// lastRestart is set for each.
func TestCoordinatedSubsystemRestart_AllPass_DeletesAll(t *testing.T) {
	pods := []v1.Pod{
		controlledOptInPod("pod-a", "uid-1"),
		controlledOptInPod("pod-b", "uid-2"),
		controlledOptInPod("pod-c", "uid-3"),
	}
	g := newTestGuardian(fake.NewSimpleClientset(toRuntimeObjs(pods...)...))
	lvolPods := map[string][]string{"lvol-1": {"uid-1", "uid-2", "uid-3"}}
	uidToPod := buildUIDToPod(pods...)

	got := g.coordinatedSubsystemRestart(context.Background(), "cid", []string{"lvol-1"}, lvolPods, uidToPod)
	if got != 3 {
		t.Fatalf("expected 3 deletions, got %d", got)
	}
	for _, uid := range []string{"uid-1", "uid-2", "uid-3"} {
		if _, ok := g.lastRestart[uid]; !ok {
			t.Errorf("lastRestart not recorded for %s", uid)
		}
	}
}

// Pods distributed across multiple sibling lvolIDs (same NQN) are all
// collected into a single candidate list and deleted together.
func TestCoordinatedSubsystemRestart_MultipleSiblings_AllDeleted(t *testing.T) {
	podA := controlledOptInPod("pod-a", "uid-1")
	podB := controlledOptInPod("pod-b", "uid-2")
	g := newTestGuardian(fake.NewSimpleClientset(&podA, &podB))

	lvolPods := map[string][]string{
		"lvol-1": {"uid-1"},
		"lvol-2": {"uid-2"},
	}
	uidToPod := buildUIDToPod(podA, podB)

	got := g.coordinatedSubsystemRestart(
		context.Background(), "cid",
		[]string{"lvol-1", "lvol-2"},
		lvolPods, uidToPod,
	)
	if got != 2 {
		t.Fatalf("expected 2 deletions across siblings, got %d", got)
	}
}

// DryRun=true: the function returns the candidate count but issues no Delete
// calls — pods must still exist in the fake client.
func TestCoordinatedSubsystemRestart_DryRun_NoDeletions(t *testing.T) {
	pods := []v1.Pod{
		controlledOptInPod("pod-a", "uid-1"),
		controlledOptInPod("pod-b", "uid-2"),
	}
	g := newTestGuardian(fake.NewSimpleClientset(toRuntimeObjs(pods...)...))
	g.cfg.DryRun = true
	lvolPods := map[string][]string{"lvol-1": {"uid-1", "uid-2"}}
	uidToPod := buildUIDToPod(pods...)

	got := g.coordinatedSubsystemRestart(context.Background(), "cid", []string{"lvol-1"}, lvolPods, uidToPod)
	if got != 2 {
		t.Fatalf("dry-run should count candidates; expected 2, got %d", got)
	}
	for _, name := range []string{"pod-a", "pod-b"} {
		if _, err := g.cs.CoreV1().Pods(testNS).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Errorf("dry-run: pod %s should still exist: %v", name, err)
		}
	}
}

// A non-NotFound delete error causes that pod to be skipped; the other pods
// in the group are still deleted and counted.
func TestCoordinatedSubsystemRestart_DeleteError_NonNotFound_SkipsPod(t *testing.T) {
	pods := []v1.Pod{
		controlledOptInPod("pod-a", "uid-1"),
		controlledOptInPod("pod-b", "uid-2"), // delete will fail
		controlledOptInPod("pod-c", "uid-3"),
	}
	fakeClient := fake.NewSimpleClientset(toRuntimeObjs(pods...)...)
	fakeClient.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.DeleteAction).GetName() == "pod-b" {
			return true, nil, errors.New("transient API error")
		}
		return false, nil, nil // fall through to default tracker for pod-a and pod-c
	})

	g := newTestGuardian(fakeClient)
	lvolPods := map[string][]string{"lvol-1": {"uid-1", "uid-2", "uid-3"}}
	uidToPod := buildUIDToPod(pods...)

	got := g.coordinatedSubsystemRestart(context.Background(), "cid", []string{"lvol-1"}, lvolPods, uidToPod)
	if got != 2 {
		t.Fatalf("expected 2 deletions (pod-b skipped on error), got %d", got)
	}
}

// A NotFound error on Delete is treated as success — the pod was already gone
// and is still counted as "deleted".
func TestCoordinatedSubsystemRestart_DeleteNotFound_CountedAsDeleted(t *testing.T) {
	pod := controlledOptInPod("pod-gone", "uid-1")
	// Pod is NOT registered in the fake client → Delete returns NotFound.
	g := newTestGuardian(fake.NewSimpleClientset())

	got := g.coordinatedSubsystemRestart(
		context.Background(), "cid",
		[]string{"lvol-1"},
		map[string][]string{"lvol-1": {"uid-1"}},
		buildUIDToPod(pod),
	)
	if got != 1 {
		t.Fatalf("NotFound should be treated as deleted; expected 1, got %d", got)
	}
}

// ─── issue #423 regression tests ─────────────────────────────────────────────

// Manifestation A: a pod tracked on two lvols must be restarted exactly once
// per tick. Before the fix, removePodFromLvolLocked deleted lastRestart[podUID]
// as soon as the first lvol's PodUIDs set emptied, even though the pod was still
// present on the second lvol. The second lvol then saw no backoff record and
// issued a second delete call for the same already-terminating pod.
func TestRestartBrokenLvols_MultiLvol_PodRestartedOnce(t *testing.T) {
	pod := controlledOptInPod("pod-a", "uid-1")
	g := newTestGuardian(fake.NewSimpleClientset(&pod))

	// Pod is tracked on two individual-subsystem lvols (simulates 2 PVCs).
	g.lvols["lvol-a"] = &LvolState{ClusterID: "cid", PodUIDs: map[string]struct{}{"uid-1": {}}}
	g.lvols["lvol-b"] = &LvolState{ClusterID: "cid", PodUIDs: map[string]struct{}{"uid-1": {}}}

	podsByLvol := map[string][]string{
		"lvol-a": {"uid-1"},
		"lvol-b": {"uid-1"},
	}

	restarted := g.restartBrokenLvols(
		context.Background(), "cid",
		[]string{"lvol-a", "lvol-b"},
		podsByLvol, buildUIDToPod(pod),
	)

	// Pod must be deleted exactly once — the second lvol must see the active backoff.
	if restarted != 1 {
		t.Errorf("expected 1 restart, got %d — double-delete bug #423-A", restarted)
	}
	// lastRestart must still be set after the tick; the bug cleared it prematurely.
	if _, ok := g.lastRestart["uid-1"]; !ok {
		t.Error("lastRestart[uid-1] was erased while pod still tracked on lvol-b (bug #423-A)")
	}
}

// Manifestation B: RegisterUnpublish must only remove the pod from the specific
// lvolID being unpublished, not from every lvol in g.lvols. Before the fix,
// unpublishing vol-A silently wiped vol-B from tracking — a subsequent break on
// vol-B would hit MarkBrokenLvol, find an unknown lvol, and silently skip it.
func TestRegisterUnpublish_OnlyRemovesTargetLvol(t *testing.T) {
	g := newTestGuardian(fake.NewSimpleClientset())

	// Pod is tracking two lvols (two mounted PVCs).
	g.lvols["lvol-a"] = &LvolState{ClusterID: "cid", PodUIDs: map[string]struct{}{"uid-1": {}}}
	g.lvols["lvol-b"] = &LvolState{ClusterID: "cid", PodUIDs: map[string]struct{}{"uid-1": {}}}

	// Unpublish only lvol-a. Target path encodes the pod UID in the standard kubelet format.
	g.RegisterUnpublish("lvol-a", "/var/lib/kubelet/pods/uid-1/volumes/kubernetes.io~csi/pvc-a/mount")

	// lvol-a must be removed.
	if _, ok := g.lvols["lvol-a"]; ok {
		t.Error("lvol-a should have been removed from g.lvols after unpublish")
	}

	// lvol-b must still be tracked — the old bug wiped it alongside lvol-a.
	st, ok := g.lvols["lvol-b"]
	if !ok {
		t.Fatal("lvol-b was incorrectly wiped from g.lvols by unpublish of lvol-a (bug #423-B)")
	}
	if _, tracked := st.PodUIDs["uid-1"]; !tracked {
		t.Error("uid-1 was incorrectly removed from lvol-b.PodUIDs (bug #423-B)")
	}
}

// A pod opted in via pod annotation (not label) is accepted by the fast path.
func TestCoordinatedSubsystemRestart_OptInViaAnnotation(t *testing.T) {
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod-ann",
			Namespace:   testNS,
			UID:         types.UID("uid-1"),
			Annotations: map[string]string{testOptInKey: testOptInValue},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "rs", Controller: boolPtr(true)},
			},
		},
	}
	g := newTestGuardian(fake.NewSimpleClientset(&pod))

	got := g.coordinatedSubsystemRestart(
		context.Background(), "cid",
		[]string{"lvol-1"},
		map[string][]string{"lvol-1": {"uid-1"}},
		buildUIDToPod(pod),
	)
	if got != 1 {
		t.Fatalf("annotation opt-in should be accepted; expected 1, got %d", got)
	}
}
