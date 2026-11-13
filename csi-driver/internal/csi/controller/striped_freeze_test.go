// What a striped volume's freeze has to guarantee on the controller side: the
// members are taken only while the volume's node holds it suspended, and
// anything short of a freeze that held for all of them is undone and retried.

package controller

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// events is an ordered log the freezer and the mock control plane both write.
type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, s)
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.log)
}

// fakeFreezer stands in for the operator's relay and the node behind it.
type fakeFreezer struct {
	events *events

	frozen    bool  // what Freeze answers
	freezeErr error // and with what error
	held      bool  // what Thaw answers
	thawErr   error

	freezeTokens, thawTokens []string
	deadline                 time.Duration
}

func (f *fakeFreezer) Freeze(_ context.Context, node, handle, token string, deadline time.Duration) (bool, error) {
	f.events.add("freeze " + node)
	f.freezeTokens = append(f.freezeTokens, token)
	f.deadline = deadline
	return f.frozen, f.freezeErr
}

func (f *fakeFreezer) Thaw(_ context.Context, node, handle, token string) (bool, error) {
	f.events.add("thaw " + node)
	f.thawTokens = append(f.thawTokens, token)
	return f.held, f.thawErr
}

// frozenServer is a striped server whose volumes are staged on worker-1 behind
// freezer, with every snapshot and clone call logged to ev.
func frozenServer(t *testing.T, mock *mockSBCLI, freezer *fakeFreezer, ev *events) *Server {
	t.Helper()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}))
	cs.attachedNodes = func(context.Context, string) ([]string, error) { return []string{"worker-1"}, nil }
	cs.freezer = freezer
	mock.failIf = func(r *http.Request) bool {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/snapshots"):
			ev.add("snapshot")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/clone"):
			ev.add("clone")
		}
		return false
	}
	return cs
}

func snapshotCount(mock *mockSBCLI) int {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	return len(mock.snapshots)
}

// The members are snapshotted between the freeze and the thaw, all of them,
// under one token.
func TestStripedSnapshotIsTakenUnderTheFreeze(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	ev := &events{}
	freezer := &fakeFreezer{events: ev, frozen: true, held: true}
	cs := frozenServer(t, mock, freezer, ev)
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))

	snap := snapshotStriped(t, cs, vol.GetVolumeId(), "snapshot-1")
	if !strings.HasSuffix(snap.GetSnapshotId(), ":s3") {
		t.Errorf("snapshot id %q", snap.GetSnapshotId())
	}
	want := []string{"freeze worker-1", "snapshot", "snapshot", "snapshot", "thaw worker-1"}
	if got := ev.all(); !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if len(freezer.freezeTokens) != 1 || !slices.Equal(freezer.freezeTokens, freezer.thawTokens) ||
		freezer.freezeTokens[0] == "" {
		t.Errorf("freeze tokens %v, thaw tokens %v: want one token, the same for both",
			freezer.freezeTokens, freezer.thawTokens)
	}
	if freezer.deadline != defaultFreezeDeadline {
		t.Errorf("deadline = %v, want %v", freezer.deadline, defaultFreezeDeadline)
	}
	if n := snapshotCount(mock); n != 3 {
		t.Errorf("%d member snapshots, want 3", n)
	}
}

// A freeze that cannot be taken takes nothing, thaws in case the node froze
// and the answer was lost, and is retryable.
func TestAFailedFreezeTakesNothing(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	ev := &events{}
	freezer := &fakeFreezer{events: ev, freezeErr: errors.New("node not linked")}
	cs := frozenServer(t, mock, freezer, ev)
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))

	_, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		SourceVolumeId: vol.GetVolumeId(), Name: "snapshot-1",
	})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("err = %v, want Unavailable", err)
	}
	if got := ev.all(); !slices.Equal(got, []string{"freeze worker-1", "thaw worker-1"}) {
		t.Errorf("events = %v, want a freeze, its thaw, and no snapshot", got)
	}
	if n := snapshotCount(mock); n != 0 {
		t.Errorf("%d member snapshots after a failed freeze", n)
	}
}

// A freeze that did not hold until the thaw, which is what the node's deadline
// does, and what a thaw under a token the node does not hold reports, makes the
// members nothing: they are deleted and the snapshot is retried.
func TestAFreezeThatDidNotHoldIsRolledBack(t *testing.T) {
	cases := map[string]*fakeFreezer{
		"the deadline thawed it first": {frozen: true, held: false},
		"the node refused the token": {frozen: true, held: false,
			thawErr: status.Error(codes.FailedPrecondition, "frozen under another token")},
	}
	for name, freezer := range cases {
		t.Run(name, func(t *testing.T) {
			mock := newMockSBCLI()
			defer mock.Close()
			ev := &events{}
			freezer.events = ev
			cs := frozenServer(t, mock, freezer, ev)
			vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))

			_, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
				SourceVolumeId: vol.GetVolumeId(), Name: "snapshot-1",
			})
			if status.Code(err) != codes.Aborted {
				t.Errorf("err = %v, want Aborted", err)
			}
			if n := snapshotCount(mock); n != 0 {
				t.Errorf("%d member snapshots survived a freeze that did not hold", n)
			}
		})
	}
}

// The members have less time than the freeze: a member snapshot still running
// when the window closes fails the attempt, before the node's deadline resumes
// the device under it.
func TestMembersSlowerThanTheFreezeAreRolledBack(t *testing.T) {
	t.Setenv(freezeDeadlineEnv, "500ms")
	mock := newMockSBCLI()
	defer mock.Close()
	ev := &events{}
	freezer := &fakeFreezer{events: ev, frozen: true, held: true}
	cs := frozenServer(t, mock, freezer, ev)
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))
	slow := strings.Split(vol.GetVolumeContext()[csicommon.CtxStripeMembers], ",")[2]
	mock.failIf = func(r *http.Request) bool {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, slow+"/snapshots") {
			mock.mu.Unlock()
			time.Sleep(time.Second)
			mock.mu.Lock()
		}
		return false
	}

	_, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		SourceVolumeId: vol.GetVolumeId(), Name: "snapshot-1",
	})
	if err == nil {
		t.Fatal("a snapshot whose member outlasted the freeze was returned")
	}
	if freezer.deadline != 500*time.Millisecond {
		t.Errorf("deadline = %v, want the configured 500ms", freezer.deadline)
	}
	// The slow member's snapshot lands after the attempt gave up, outside any
	// freeze. The retry clears it rather than reusing it.
	time.Sleep(700 * time.Millisecond)
	mock.mu.Lock()
	late := ""
	for id := range mock.snapshots {
		late = id
	}
	n := len(mock.snapshots)
	mock.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d member snapshots after the late one landed, want just it", n)
	}
	mock.failIf = nil
	snapshotStriped(t, cs, vol.GetVolumeId(), "snapshot-1")
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.snapshots) != 3 || mock.snapshots[late] != nil {
		t.Errorf("after the retry: %d member snapshots, late one kept: %v; want 3 fresh ones",
			len(mock.snapshots), mock.snapshots[late] != nil)
	}
}

// A volume attached nowhere has no writer: no freeze, and the snapshot is taken.
func TestAnUnattachedStripedVolumeNeedsNoFreeze(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	ev := &events{}
	freezer := &fakeFreezer{events: ev}
	cs := frozenServer(t, mock, freezer, ev)
	cs.attachedNodes = func(context.Context, string) ([]string, error) { return nil, nil }
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "2", 2*gib))

	snapshotStriped(t, cs, vol.GetVolumeId(), "snapshot-1")
	if got := ev.all(); slices.Contains(got, "freeze worker-1") {
		t.Errorf("an unattached volume was frozen: %v", got)
	}
}

// A node that has the volume attached and not staged answers that it froze
// nothing, and nothing there writes to it: the snapshot is taken, and there is
// no freeze to thaw.
func TestAnAttachedUnstagedVolumeIsTakenWithoutAFreeze(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	ev := &events{}
	freezer := &fakeFreezer{events: ev, frozen: false}
	cs := frozenServer(t, mock, freezer, ev)
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "2", 2*gib))

	snapshotStriped(t, cs, vol.GetVolumeId(), "snapshot-1")
	if got := ev.all(); !slices.Equal(got, []string{"freeze worker-1", "snapshot", "snapshot"}) {
		t.Errorf("events = %v", got)
	}
}

func TestAStripedVolumeOnTwoNodesIsRefused(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	ev := &events{}
	cs := frozenServer(t, mock, &fakeFreezer{events: ev}, ev)
	cs.attachedNodes = func(context.Context, string) ([]string, error) { return []string{"a", "b"}, nil }
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "2", 2*gib))

	_, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		SourceVolumeId: vol.GetVolumeId(), Name: "snapshot-1",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("err = %v, want FailedPrecondition", err)
	}
	if len(ev.all()) != 0 {
		t.Errorf("events = %v, want nothing frozen or taken", ev.all())
	}
}

// Without the link there is no way to stop the writer, so an attached volume is
// not snapshotted.
func TestAnAttachedVolumeWithoutTheLinkIsNotSnapshotted(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	ev := &events{}
	cs := frozenServer(t, mock, &fakeFreezer{events: ev}, ev)
	cs.freezer = nil
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "2", 2*gib))

	_, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		SourceVolumeId: vol.GetVolumeId(), Name: "snapshot-1",
	})
	if status.Code(err) != codes.Unavailable || snapshotCount(mock) != 0 {
		t.Errorf("err = %v with %d snapshots, want Unavailable and none", err, snapshotCount(mock))
	}
}

// Member snapshots an earlier attempt left are not reused, since they were not
// taken under this freeze: they are replaced.
func TestLeftoverMemberSnapshotsAreRetaken(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	ev := &events{}
	cs := frozenServer(t, mock, &fakeFreezer{events: ev, frozen: true, held: true}, ev)
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "2", 2*gib))
	first := snapshotStriped(t, cs, vol.GetVolumeId(), "snapshot-1")
	again := snapshotStriped(t, cs, vol.GetVolumeId(), "snapshot-1")
	if first.GetSnapshotId() == again.GetSnapshotId() {
		t.Error("the retry reused a member snapshot from an earlier attempt")
	}
	if n := snapshotCount(mock); n != 2 {
		t.Errorf("%d member snapshots, want the 2 of the retry", n)
	}
}

// A clone of a striped volume that may be in use is taken under its freeze,
// and is undone when the freeze did not hold.
func TestCloneOfALiveStripeIsTakenUnderTheFreeze(t *testing.T) {
	cloneReq := func(source string) *csi.CreateVolumeRequest {
		req := stripedRequest("pvc-clone", "2", 2*gib)
		req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
			Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: source},
		}}
		return req
	}

	mock := newMockSBCLI()
	defer mock.Close()
	ev := &events{}
	freezer := &fakeFreezer{events: ev, frozen: true, held: true}
	cs := frozenServer(t, mock, freezer, ev)
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "2", 2*gib))

	createStriped(t, cs, cloneReq(vol.GetVolumeId()))
	want := []string{"freeze worker-1", "clone", "clone", "thaw worker-1"}
	if got := ev.all(); !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}

	// A second clone whose freeze lapsed leaves no member behind.
	freezer.held = false
	req := cloneReq(vol.GetVolumeId())
	req.Name = "pvc-clone-2"
	if _, err := cs.CreateVolume(context.Background(), req); status.Code(err) != codes.Aborted {
		t.Errorf("err = %v, want Aborted", err)
	}
	byName := volumesNamed(mock)
	if byName["pvc-clone-2"] != nil || byName["pvc-clone-2-s1"] != nil {
		t.Errorf("a clone whose freeze lapsed left members: %v", byName)
	}
}

// Where a volume is staged is read off its VolumeAttachments, through its
// PersistentVolume, counting an attachment being torn down until it is gone.
func TestStagedNodesReadTheAttachments(t *testing.T) {
	const handle = "c:p:v:s2"
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: handle},
		}},
	}
	attachment := func(name, node, pvName string, attached bool) *storagev1.VolumeAttachment {
		return &storagev1.VolumeAttachment{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: storagev1.VolumeAttachmentSpec{
				NodeName: node,
				Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &pvName},
			},
			Status: storagev1.VolumeAttachmentStatus{Attached: attached},
		}
	}
	cs := &Server{kubeClient: fake.NewSimpleClientset(pv,
		attachment("a", "worker-1", "pv-1", true),
		attachment("b", "worker-2", "pv-1", false),
		attachment("c", "worker-3", "pv-other", true),
	)}
	nodes, err := cs.stagedNodes(context.Background(), handle)
	if err != nil || !slices.Equal(nodes, []string{"worker-1"}) {
		t.Errorf("stagedNodes = %v, %v; want worker-1", nodes, err)
	}
}
