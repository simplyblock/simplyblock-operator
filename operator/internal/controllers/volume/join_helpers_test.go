// The scaffolding the JoinConsistencyGroup tests share: a fake control plane
// for the join and move calls, fake CSI nodes serving the swap service, and the
// objects a join of two volumes on two hosts needs.
//
// Every fake writes to one ordered log, because the property most of these
// tests exist for is an order: every host holds its I/O before any namespace
// moves, and no host is released before the namespaces are somewhere.

package volume

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack/swaprpc"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	joinGroupName = "orders"
	joinGroupID   = "77777777-7777-4777-8777-777777777777"
	joinOpsName   = "cg-join-" + testVolumeID

	// The second member, already in the group, on its own host.
	memberVolumeID = "88888888-8888-4888-8888-888888888888"
	memberPVName   = "pvc-" + memberVolumeID

	joinMoveID    = "move-1"
	joinSourceNQN = "nqn.2023-02.io.simplyblock:cluster:lvol:joiner"
	memberNQN     = "nqn.2023-02.io.simplyblock:cluster:lvol:group"
	joinTargetNQN = "nqn.2023-02.io.simplyblock:cluster:lvol:fresh"

	joinHost   = "worker-2"
	memberHost = "worker-3"
)

// callLog is the one ordered record every fake appends to.
type callLog struct{ calls []string }

func (l *callLog) add(format string, args ...any) {
	l.calls = append(l.calls, fmt.Sprintf(format, args...))
}

// index is the position of the first call with the prefix, or -1.
func (l *callLog) index(prefix string) int {
	return slices.IndexFunc(l.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// last is the position of the last call with the prefix, or -1.
func (l *callLog) last(prefix string) int {
	for i := len(l.calls) - 1; i >= 0; i-- {
		if strings.HasPrefix(l.calls[i], prefix) {
			return i
		}
	}
	return -1
}

func (l *callLog) count(prefix string) int {
	n := 0
	for _, c := range l.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// fakeJoinAPI answers the join and move calls from fields a test sets.
type fakeJoinAPI struct {
	log *callLog

	groupMissing bool

	plan    controlplane.JoinPlan
	planErr error

	joinErr error

	move       controlplane.SubsystemMove
	createErr  error
	requestIDs []string

	prepareErr error
	// prepareHosts is the host NQN each prepare named, in call order.
	prepareHosts []string

	// startErr is what the move call answers with. A nil one moves every
	// namespace to the target.
	startErr  error
	deadlines []time.Time

	// commitErrs are answered in turn, then success.
	commitErrs  []error
	rollbackErr error

	// leave is what a leave answers with, and leaveErr its refusal.
	leave    controlplane.SubsystemMove
	leaveErr error

	// detachErrs are answered in turn, then success, and detached counts the
	// detaches that succeeded. onDetach runs after each of those.
	detachErrs []error
	detached   int
	onDetach   func()

	// moveGroups is every subsystem-move call, each as the call's name and the
	// group it addressed. It is how a test tells a move on the group the volume
	// leaves from one on the group it joins.
	moveGroups []string
}

// byID is the move a call addresses: the leave or the join's.
func (f *fakeJoinAPI) byID(moveID string) controlplane.SubsystemMove {
	if moveID != "" && moveID == f.leave.ID {
		return f.leave
	}
	return f.move
}

func (f *fakeJoinAPI) onGroup(call, groupID string) {
	f.moveGroups = append(f.moveGroups, call+" "+groupID)
}

func (f *fakeJoinAPI) CreateSubsystemLeave(
	_ context.Context, _, groupID, _, requestID string,
) (controlplane.SubsystemMove, error) {
	f.log.add("leave")
	f.onGroup("leave", groupID)
	f.requestIDs = append(f.requestIDs, requestID)
	if f.leaveErr != nil {
		return controlplane.SubsystemMove{}, f.leaveErr
	}
	return f.leave, nil
}

func (f *fakeJoinAPI) DetachConsistencyGroupMember(_ context.Context, _, groupID, _ string) error {
	f.log.add("detach %s", groupID)
	if len(f.detachErrs) > 0 {
		err := f.detachErrs[0]
		f.detachErrs = f.detachErrs[1:]
		if err != nil {
			return err
		}
	}
	f.detached++
	if f.onDetach != nil {
		f.onDetach()
	}
	return nil
}

func (f *fakeJoinAPI) ConsistencyGroupNamed(
	_ context.Context, _, name string,
) (controlplane.ConsistencyGroup, bool, error) {
	f.log.add("group %s", name)
	if f.groupMissing {
		return controlplane.ConsistencyGroup{}, false, nil
	}
	return controlplane.ConsistencyGroup{ID: joinGroupID, Name: name, MemberCount: 1}, true, nil
}

func (f *fakeJoinAPI) PlanConsistencyGroupJoin(context.Context, string, string, string) (controlplane.JoinPlan, error) {
	f.log.add("plan")
	return f.plan, f.planErr
}

func (f *fakeJoinAPI) JoinConsistencyGroupMember(context.Context, string, string, string) error {
	f.log.add("join")
	return f.joinErr
}

func (f *fakeJoinAPI) CreateSubsystemMove(
	_ context.Context, _, groupID, _, requestID string,
) (controlplane.SubsystemMove, error) {
	f.log.add("create")
	f.onGroup("create", groupID)
	f.requestIDs = append(f.requestIDs, requestID)
	if f.createErr != nil {
		return controlplane.SubsystemMove{}, f.createErr
	}
	return f.move, nil
}

func (f *fakeJoinAPI) GetSubsystemMove(_ context.Context, _, _, moveID string) (controlplane.SubsystemMove, error) {
	return f.byID(moveID), nil
}

// PrepareSubsystemMove answers the way the control plane does: the target's
// paths only for a named host, since the connect entries are a host's own, and
// none for a call that names no host.
func (f *fakeJoinAPI) PrepareSubsystemMove(
	_ context.Context, _, groupID, moveID, hostNQN string,
) (controlplane.PreparedMove, error) {
	f.log.add("prepare")
	f.onGroup("prepare", groupID)
	f.prepareHosts = append(f.prepareHosts, hostNQN)
	if f.prepareErr != nil {
		return controlplane.PreparedMove{}, f.prepareErr
	}
	move := f.byID(moveID)
	if hostNQN == "" {
		return controlplane.PreparedMove{Move: move, Target: lvol.Connection{NQN: move.TargetNQN}}, nil
	}
	return controlplane.PreparedMove{Move: move, Target: lvol.Connection{
		NQN: move.TargetNQN,
		Endpoints: []lvol.Endpoint{
			{Transport: "tcp", Address: "10.0.0.1", Port: 4420},
			{Transport: "tcp", Address: "10.0.0.2", Port: 4420},
		},
	}}, nil
}

func (f *fakeJoinAPI) StartSubsystemMove(
	_ context.Context, _, groupID, moveID string, deadline time.Time,
) (controlplane.SubsystemMove, error) {
	f.log.add("move")
	f.onGroup("move", groupID)
	f.deadlines = append(f.deadlines, deadline)
	if f.startErr != nil {
		return controlplane.SubsystemMove{}, f.startErr
	}
	moved := f.byID(moveID)
	moved.NamespacesAt = controlplane.NamespacesAtTarget
	return moved, nil
}

func (f *fakeJoinAPI) CommitSubsystemMove(_ context.Context, _, groupID, _ string) error {
	f.log.add("commit")
	f.onGroup("commit", groupID)
	if len(f.commitErrs) > 0 {
		err := f.commitErrs[0]
		f.commitErrs = f.commitErrs[1:]
		return err
	}
	return nil
}

func (f *fakeJoinAPI) RollbackSubsystemMove(_ context.Context, _, groupID, _ string) error {
	f.log.add("rollback")
	f.onGroup("rollback", groupID)
	return f.rollbackErr
}

// fakeSwapNode is one CSI node's swap service. It keeps the mappings it holds,
// so a resume onto a subsystem is visible as where a volume points.
type fakeSwapNode struct {
	name string
	log  *callLog

	instance string

	notSwappable map[string]bool
	suspendErr   error
	resumeErrs   []error

	suspended map[string]bool
	pointsAt  map[string]string
	deadline  time.Time
}

func (n *fakeSwapNode) Describe(_ context.Context, handles []string) ([]swaprpc.VolumeState, error) {
	n.log.add("describe %s", n.name)
	out := make([]swaprpc.VolumeState, 0, len(handles))
	for _, h := range handles {
		out = append(out, swaprpc.VolumeState{
			Handle: h, Staged: true, Swappable: !n.notSwappable[h],
			Suspended: n.suspended[h], NQN: n.pointsAt[h],
		})
	}
	return out, nil
}

func (n *fakeSwapNode) Prepare(
	_ context.Context, handles []string, targetNQN string, paths []nvmeof.Target,
) ([]swaprpc.VolumeState, error) {
	n.log.add("prepare %s %s %d", n.name, targetNQN, len(paths))
	return nil, nil
}

func (n *fakeSwapNode) Suspend(_ context.Context, handles []string, deadline time.Time) ([]swaprpc.VolumeState, error) {
	n.log.add("suspend %s", n.name)
	if n.suspendErr != nil {
		return nil, n.suspendErr
	}
	n.deadline = deadline
	for _, h := range handles {
		n.suspended[h] = true
	}
	return nil, nil
}

func (n *fakeSwapNode) Resume(_ context.Context, handles []string, nqn string) ([]swaprpc.VolumeState, error) {
	n.log.add("resume %s %s", n.name, nqn)
	if len(n.resumeErrs) > 0 {
		err := n.resumeErrs[0]
		n.resumeErrs = n.resumeErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	for _, h := range handles {
		n.suspended[h] = false
		n.pointsAt[h] = nqn
	}
	return nil, nil
}

func (n *fakeSwapNode) Release(_ context.Context, nqn string) (bool, error) {
	n.log.add("release %s %s", n.name, nqn)
	return true, nil
}

// fakeSwapPeers is the link registry: the nodes that are linked, and those
// without the swap capability.
type fakeSwapPeers struct {
	nodes       map[string]*fakeSwapNode
	incapable   map[string]bool
	unreachable map[string]bool
}

func (p *fakeSwapPeers) Node(name string) (SwapNode, string, error) {
	if p.unreachable[name] {
		return nil, "", fmt.Errorf("dial %s: connection refused", name)
	}
	node, ok := p.nodes[name]
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", ErrNoSwapPeer, name)
	}
	if p.incapable[name] {
		return nil, "", fmt.Errorf("%w: %s", ErrNoSwapCapability, name)
	}
	return node, node.instance, nil
}

// joinWorld is one join test's fakes and reconciler.
type joinWorld struct {
	t     *testing.T
	log   *callLog
	api   *fakeJoinAPI
	cp    *fakeControlPlane
	peers *fakeSwapPeers
	r     *PersistentVolumeOpsReconciler
	now   time.Time
}

// joinVolume is a PersistentVolume of the test cluster's pool with a claim.
func joinVolume(name, volumeID, claim string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       CSIDriverName,
					VolumeHandle: string(lvol.NewVolumeHandle(testClusterID, testPoolID, volumeID)),
				},
			},
			ClaimRef:    &corev1.ObjectReference{Namespace: "app", Name: claim},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
		},
	}
}

func attachment(pv, node string) *storagev1.VolumeAttachment {
	return &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "va-" + pv},
		Spec: storagev1.VolumeAttachmentSpec{
			Attacher: CSIDriverName,
			NodeName: node,
			Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &pv},
		},
		Status: storagev1.VolumeAttachmentStatus{Attached: true},
	}
}

// hostNode is a consuming host's Node, carrying the host NQN its CSI node
// plugin publishes.
func hostNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: name, UID: types.UID("uid-" + name),
		Annotations: map[string]string{kube.AnnoNVMeHostNQN: hostNQNOf(name)},
	}}
}

func hostNQNOf(node string) string { return "nqn.2014-08.org.nvmexpress:uuid:" + node }

// joinOps is a join of the test volume to the test group.
func joinOps() *simplyblockv1alpha2.PersistentVolumeOps {
	timeout := int32(10)
	return &simplyblockv1alpha2.PersistentVolumeOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:       joinOpsName,
			UID:        types.UID("uid-" + joinOpsName),
			Finalizers: []string{opsFinalizer},
		},
		Spec: simplyblockv1alpha2.PersistentVolumeOpsSpec{
			PersistentVolumeName: testPVName,
			Action:               simplyblockv1alpha2.PersistentVolumeOpsActionJoinConsistencyGroup,
			JoinConsistencyGroup: &simplyblockv1alpha2.JoinConsistencyGroupSpec{
				GroupName:             joinGroupName,
				SuspendTimeoutSeconds: &timeout,
			},
		},
	}
}

// wholeGroupMove is a move of the joiner and the existing member into a fresh
// subsystem.
func wholeGroupMove() controlplane.SubsystemMove {
	return controlplane.SubsystemMove{
		ID: joinMoveID, TargetNQN: joinTargetNQN, TargetIsNew: true,
		NamespacesAt: controlplane.NamespacesAtSource,
		Volumes: []controlplane.MovingNamespace{
			{LvolID: testVolumeID, SourceNQN: joinSourceNQN, SourceNSID: 1, NSUUID: testVolumeID, TargetNSID: 1},
			{LvolID: memberVolumeID, SourceNQN: memberNQN, SourceNSID: 1, NSUUID: memberVolumeID, TargetNSID: 2},
		},
	}
}

// newJoinWorld is an on-pin join of the test volume into a full group whose
// one member sits on another host, so the whole group moves to a fresh
// subsystem and both hosts take part.
func newJoinWorld(t *testing.T, extra ...client.Object) *joinWorld {
	t.Helper()
	log := &callLog{}
	w := &joinWorld{t: t, log: log, now: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)}
	w.api = &fakeJoinAPI{
		log: log,
		plan: controlplane.JoinPlan{
			Steps: []string{controlplane.JoinStepJoin, controlplane.JoinStepColocate},
			Colocation: &controlplane.Colocation{
				TargetNQN: joinTargetNQN, TargetIsNew: true,
				MovingLvolIDs: []string{testVolumeID, memberVolumeID},
			},
		},
		move: wholeGroupMove(),
	}
	w.cp = &fakeControlPlane{
		volume: lvol.Volume{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, testVolumeID), NQN: joinSourceNQN},
		members: []lvol.Volume{
			{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, testVolumeID), NQN: joinSourceNQN},
			{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, memberVolumeID), NQN: memberNQN},
		},
	}
	node := func(name string) *fakeSwapNode {
		return &fakeSwapNode{
			name: name, log: log, instance: "instance-" + name,
			notSwappable: map[string]bool{}, suspended: map[string]bool{}, pointsAt: map[string]string{},
		}
	}
	w.peers = &fakeSwapPeers{
		nodes:       map[string]*fakeSwapNode{joinHost: node(joinHost), memberHost: node(memberHost)},
		incapable:   map[string]bool{},
		unreachable: map[string]bool{},
	}

	objs := make([]client.Object, 0, 7+len(extra))
	objs = append(objs,
		joinOps(),
		joinVolume(testPVName, testVolumeID, "joiner"),
		joinVolume(memberPVName, memberVolumeID, "member"),
		attachment(testPVName, joinHost),
		attachment(memberPVName, memberHost),
		hostNode(joinHost),
		hostNode(memberHost),
		testClusterObject(),
		testNodeObject(),
	)
	objs = append(objs, extra...)
	w.r = testReconciler(t, w.cp, objs...)
	w.r.Join = w.api
	w.r.Swap = w.peers
	w.r.Clock = func() time.Time { return w.now }
	return w
}

// pass runs one reconcile of the join.
func (w *joinWorld) pass() (ctrl.Result, error) {
	w.t.Helper()
	return w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: joinOpsName}})
}

// run reconciles until the operation is terminal or n passes have run, and
// returns the operation.
func (w *joinWorld) run(n int) *simplyblockv1alpha2.PersistentVolumeOps {
	w.t.Helper()
	for range n {
		if _, err := w.pass(); err != nil {
			w.t.Fatalf("reconcile: %v", err)
		}
		if ops := w.ops(); terminal(ops.Status.Phase) {
			return ops
		}
	}
	return w.ops()
}

// runUntil reconciles until the operation reaches the step, and fails the test
// if it does not within n passes.
func (w *joinWorld) runUntil(s step, n int) *simplyblockv1alpha2.PersistentVolumeOps {
	w.t.Helper()
	for range n {
		if ops := w.ops(); ops.Status.Step.State == string(s) {
			return ops
		}
		if _, err := w.pass(); err != nil {
			w.t.Fatalf("reconcile: %v", err)
		}
	}
	ops := w.ops()
	if ops.Status.Step.State != string(s) {
		w.t.Fatalf("the operation is at %q (%s: %s), never reached %s",
			ops.Status.Step.State, ops.Status.Phase, ops.Status.Message, s)
	}
	return ops
}

func (w *joinWorld) ops() *simplyblockv1alpha2.PersistentVolumeOps {
	w.t.Helper()
	var ops simplyblockv1alpha2.PersistentVolumeOps
	if err := w.r.Get(context.Background(), types.NamespacedName{Name: joinOpsName}, &ops); err != nil {
		w.t.Fatalf("read the operation: %v", err)
	}
	return &ops
}

func (w *joinWorld) pv(name string) *corev1.PersistentVolume {
	w.t.Helper()
	var pv corev1.PersistentVolume
	if err := w.r.Get(context.Background(), types.NamespacedName{Name: name}, &pv); err != nil {
		w.t.Fatalf("read volume %s: %v", name, err)
	}
	return &pv
}

// update applies a change to the operation's spec or metadata.
func (w *joinWorld) update(change func(*simplyblockv1alpha2.PersistentVolumeOps)) {
	w.t.Helper()
	ops := w.ops()
	change(ops)
	if err := w.r.Update(context.Background(), ops); err != nil {
		w.t.Fatalf("update the operation: %v", err)
	}
}

// events drains the recorder.
func (w *joinWorld) events() []string {
	recorder := w.r.Recorder.(*events.FakeRecorder)
	var out []string
	for {
		select {
		case e := <-recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func hasEvent(recorded []string, reason string) bool {
	return slices.ContainsFunc(recorded, func(e string) bool { return strings.Contains(e, " "+reason+" ") })
}

// eventWith is the first event of the reason, or the empty string.
func eventWith(recorded []string, reason string) string {
	for _, e := range recorded {
		if strings.Contains(e, " "+reason+" ") {
			return e
		}
	}
	return ""
}

func handleOf(volumeID string) string {
	return string(lvol.NewVolumeHandle(testClusterID, testPoolID, volumeID))
}

// statusErr is a transient control-plane failure.
var statusErr = &controlplane.StatusError{Op: "test", StatusCode: 503}
