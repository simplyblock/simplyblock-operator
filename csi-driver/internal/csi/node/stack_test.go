// What each node RPC does to a volume's stack.
//
// These assert the verb and the plan, not the device: which layers a volume is
// made of, which runner call an RPC chose, and what it did not call. The last
// of those is the one that matters most, because an unstage that reached for
// Destroy would take a volume's data with it, and no fake device can make that
// claim provable.

package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"
	k8smount "k8s.io/mount-utils"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"

	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/mount"
)

// recordingRunner answers every verb without touching a host and logs which one
// it was asked for.
type recordingRunner struct {
	calls []string
	plans map[string][]string

	upErr      error
	downErr    error
	healErr    error
	growErr    error
	destroyErr error

	exposes string
}

func newRecordingRunner() *recordingRunner {
	return &recordingRunner{plans: map[string][]string{}, exposes: fakeDevice}
}

func (r *recordingRunner) note(verb string, plan volstack.Plan) {
	r.calls = append(r.calls, verb)
	r.plans[verb] = plan.Names()
}

func (r *recordingRunner) artifact() volstack.Artifact {
	if r.exposes == "" {
		return volstack.Artifact{}
	}
	return volstack.Artifact{Devices: []blockdev.Device{{Path: r.exposes, Name: filepath.Base(r.exposes)}}}
}

func (r *recordingRunner) Up(_ context.Context, _ string, plan volstack.Plan) (volstack.Artifact, error) {
	r.note("up", plan)
	return r.artifact(), r.upErr
}

func (r *recordingRunner) Down(_ context.Context, _ string, plan volstack.Plan) error {
	r.note("down", plan)
	return r.downErr
}

func (r *recordingRunner) Heal(_ context.Context, _ string, plan volstack.Plan) error {
	r.note("heal", plan)
	return r.healErr
}

func (r *recordingRunner) Grow(_ context.Context, plan volstack.Plan) error {
	r.note("grow", plan)
	return r.growErr
}

func (r *recordingRunner) Destroy(_ context.Context, _ string, plan volstack.Plan) error {
	r.note("destroy", plan)
	return r.destroyErr
}

func (r *recordingRunner) Observe(_ context.Context, plan volstack.Plan) (volstack.Artifact, error) {
	r.note("observe", plan)
	return r.artifact(), nil
}

// called reports whether the runner was asked for verb.
func (r *recordingRunner) called(verb string) bool {
	for _, call := range r.calls {
		if call == verb {
			return true
		}
	}
	return false
}

// newStackedServer is a node server whose stack answers from runner and whose
// mounter drives a fake kernel. Nothing here reaches a control plane, so every
// plan is built from the stashed context.
func newStackedServer(t *testing.T, runner stackRunner) (*Server, string) {
	t.Helper()
	s, recordDir := newTestStack(t, runner)
	return &Server{
		mounter:     mount.NewWith(k8smount.NewFakeMounter(nil), nil),
		stack:       s,
		volumeLocks: csicommon.NewVolumeLocks(),
	}, recordDir
}

// newTestStack is the stack with its record directory in a temporary one, so a
// test can write a record and see the teardown read it. The directory is
// returned because a test that corrupts a record writes the file itself.
func newTestStack(t *testing.T, runner stackRunner) (*stack, string) {
	t.Helper()
	recordDir := t.TempDir()
	s := newStack(mount.NewWith(k8smount.NewFakeMounter(nil), nil), recordDir)
	s.runner = runner
	return s, recordDir
}

// plainShape is the layer list a filesystem volume stages as, rendered the way
// these tests compare plans.
const plainShape = "fabric → filesystem"

// lvmShape is the layer list a volume carrying client-side compression or
// deduplication stages as, rendered the same way.
const lvmShape = "fabric → lvmPhysicalVolume → lvmVolumeGroup → lvmLogicalVolume → filesystem"

// stagedContext is the volume context a staged volume leaves behind, which is
// all the teardown and expansion paths have to work from.
func stagedContext() map[string]string {
	return map[string]string{
		"nqn":         "nqn.2023-05.io.simplyblock:lvol:" + pvcTestLvol,
		"nsId":        "1",
		"uuid":        pvcTestLvol,
		"targetType":  "tcp",
		"connections": `[{"ip":"10.0.0.1","port":4420}]`,
		"devicePath":  fakeDevice,
	}
}

// stashedAt writes a staged volume's context where the node service looks for
// it, and returns the staging parent path.
func stashedAt(t *testing.T, vc map[string]string) string {
	t.Helper()
	parent := t.TempDir()
	if err := stashVolumeContext(vc, parent); err != nil {
		t.Fatalf("stash volume context: %v", err)
	}
	return parent
}

// A mount volume's stack is the fabric with a filesystem above it, and a raw
// block volume's is the fabric alone. Raw block mode is a shorter plan rather
// than a flag inside a stage function, which is what keeps a block volume from
// sharing a code path with a formatting one.
func TestPlanForSelectsTheShapeFromTheCapability(t *testing.T) {
	s, _ := newTestStack(t, newRecordingRunner())
	node := s.node("", nil)
	volume := stackVolume("/staging", stagedContext(), mountCapability())

	vc := stagedContext()
	options := vdoOptions(vc)

	mountPlan := planFor(node, connectionFromContext(vc), volume, options, shapeFor(vc, mountCapability()))
	if got := strings.Join(mountPlan.Names(), " → "); got != plainShape {
		t.Errorf("a filesystem volume stages as %s", got)
	}

	blockPlan := planFor(node, connectionFromContext(vc), volume, options, shapeFor(vc, blockCapability()))
	if got := strings.Join(blockPlan.Names(), " → "); got != "fabric" {
		t.Errorf("a raw block volume stages as %s, and nothing may format it", got)
	}
}

// Every path the control plane published is carried, in the order it published
// them, so the primary stays first. The DHCHAP key material rides along with
// them, because the connect line is the only channel the control plane has for
// it and a plan that dropped it would attach an authenticated subsystem with no
// credentials.
func TestConnectionFromResponsesCarriesEveryPathAndItsCredentials(t *testing.T) {
	responses := []*controlplane.LvolConnectResp{
		{
			Nqn: "nqn.2023-05.io.simplyblock:lvol:" + pvcTestLvol, NSID: 3,
			IP: "10.0.0.1", Port: 4420, TargetType: "TCP", NrIoQueues: 4, ReconnectDelay: 2,
			Connect: "nvme connect --hostnqn=nqn.2014-08.org.nvmexpress:uuid:abc " +
				"--dhchap-secret=DHHC-1:00:secret: --dhchap-ctrl-secret=DHHC-1:00:ctrl: --tls",
		},
		{
			Nqn: "nqn.2023-05.io.simplyblock:lvol:" + pvcTestLvol, NSID: 3,
			IP: "10.0.0.2", Port: 4420, TargetType: "TCP",
			Connect: "nvme connect --hostnqn=nqn.2014-08.org.nvmexpress:uuid:abc",
		},
	}

	connection, hostNQN := connectionFromResponses(responses, pvcTestLvol)

	if len(connection.Endpoints) != 2 {
		t.Fatalf("the connection carries %d endpoints, want both published paths", len(connection.Endpoints))
	}
	if connection.Endpoints[0].Address != "10.0.0.1" {
		t.Errorf("the primary path is %s, and the control plane's order is the attach order",
			connection.Endpoints[0].Address)
	}
	if connection.NSID != 3 || connection.UUID != pvcTestLvol {
		t.Errorf("the connection names namespace %d of %s, want 3 of %s",
			connection.NSID, connection.UUID, pvcTestLvol)
	}
	if connection.Endpoints[0].DHCHAPSecret == "" || connection.Endpoints[0].DHCHAPCtrlSecret == "" {
		t.Error("the DHCHAP key material was dropped, so an authenticated connect would be refused")
	}
	if !connection.Endpoints[0].TLS {
		t.Error("the connection is not marked encrypted, so the volume would be attached in the clear")
	}
	if hostNQN != "nqn.2014-08.org.nvmexpress:uuid:abc" {
		t.Errorf("the host identity is %q, and presenting another is what an allowed-hosts pool refuses", hostNQN)
	}
}

// The teardown path asks the control plane nothing: an unstage has to work when
// the control plane does not, and a release attaches nothing, so it needs no
// credentials. What it does need is the identity of the namespace, which the
// stashed context carries.
func TestConnectionFromContextNamesTheNamespace(t *testing.T) {
	vc := stagedContext()
	vc["targetLvolID"] = "11111111-2222-3333-4444-555555555555"

	connection := connectionFromContext(vc)

	if connection.NQN != vc["nqn"] || connection.NSID != 1 {
		t.Errorf("the connection names %s namespace %d", connection.NQN, connection.NSID)
	}
	if connection.UUID != vc["targetLvolID"] {
		t.Errorf("the connection identifies the namespace as %s, and after a failover the data is "+
			"served out of the clone", connection.UUID)
	}
	if len(connection.Endpoints) != 1 || connection.Endpoints[0].Address != "10.0.0.1" {
		t.Errorf("the endpoints did not survive the round trip: %+v", connection.Endpoints)
	}
}

// A stage brings the stack up and records what the volume now is, so the later
// RPCs act on a device that was read rather than guessed.
func TestStageBringsTheStackUp(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)

	parent := t.TempDir()
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: parent,
		VolumeCapability:  mountCapability(),
		VolumeContext:     stagedContext(),
	}

	if _, err := ns.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	if !runner.called("up") {
		t.Fatalf("staging did not bring the stack up; it called %v", runner.calls)
	}
	if runner.called("destroy") {
		t.Fatal("staging destroyed something")
	}

	stashed, err := lookupVolumeContext(parent)
	if err != nil {
		t.Fatalf("the staged volume left no context behind: %v", err)
	}
	if stashed["devicePath"] != fakeDevice {
		t.Errorf("the staged device was recorded as %q, want the one the stack exposed", stashed["devicePath"])
	}
	if stashed[stagedFsTypeKey] != extFS {
		t.Errorf("the staged filesystem was recorded as %q, and a restage has nothing else to go on",
			stashed[stagedFsTypeKey])
	}
}

// An unstage releases and never destroys. It fires whenever no pod on this node
// needs the volume mounted, which includes an ordinary pod restart, and
// conflating the two verbs is what once ran vgremove over a volume holding
// data.
func TestUnstageReleasesAndNeverDestroys(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)
	parent := stashedAt(t, stagedContext())

	_, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: parent,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}

	if !runner.called("down") {
		t.Fatalf("the unstage released nothing; it called %v", runner.calls)
	}
	if runner.called("destroy") {
		t.Fatal("the unstage destroyed the volume's objects, and an unstage is not a deletion")
	}
}

// A volume on its way out takes the node-local objects its stack built with it,
// because nothing will stage it again and nothing else on the host knows they
// are there. Down comes first: what is released is the host's hold, and what is
// destroyed is what is left after it.
func TestUnstageDestroysAVolumeThatIsBeingDeleted(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newPVCTestNodeServer(t, deletingPV(), annotatedPVC(""))
	ns.mounter = mount.NewWith(k8smount.NewFakeMounter(nil), nil)
	ns.stack, _ = newTestStack(t, runner)
	ns.volumeLocks = csicommon.NewVolumeLocks()

	parent := stashedAt(t, stagedContext())
	_, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: parent,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}

	if !runner.called("destroy") {
		t.Fatalf("the deleted volume's node-local objects were left behind; the unstage called %v", runner.calls)
	}
	if got := strings.Join(runner.calls, " "); !strings.Contains(got, "down destroy") {
		t.Errorf("the verbs ran as %q, and a destroy before the release would remove an object still held", got)
	}
}

// A volume whose class says its storage is retained is never destroyed, however
// else it looks. Of the two ways to be wrong here, only one is unrecoverable.
func TestUnstageKeepsARetainedVolume(t *testing.T) {
	runner := newRecordingRunner()
	pv := deletingPV()
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain

	ns, _ := newPVCTestNodeServer(t, pv, annotatedPVC(""))
	ns.mounter = mount.NewWith(k8smount.NewFakeMounter(nil), nil)
	ns.stack, _ = newTestStack(t, runner)
	ns.volumeLocks = csicommon.NewVolumeLocks()

	parent := stashedAt(t, stagedContext())
	if _, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: parent,
	}); err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}

	if runner.called("destroy") {
		t.Fatal("a retained volume's objects were destroyed on an ordinary unstage")
	}
}

// The teardown follows the record rather than the class, because a class can be
// edited or deleted after a volume is provisioned and a teardown owes the truth
// about what is on the host.
func TestTeardownPlanFollowsTheRecord(t *testing.T) {
	cases := []struct {
		name    string
		layers  []string
		want    string
		wantErr string
	}{
		{name: "raw block", layers: []string{"fabric"}, want: "fabric"},
		{name: "filesystem", layers: []string{"fabric", "filesystem"}, want: plainShape},
		{name: "client-side compression", layers: strings.Split(lvmShape, " → "), want: lvmShape},
		{
			name:    "a layer this build does not know",
			layers:  []string{"fabric", "dmCrypt", "filesystem"},
			wantErr: "dmCrypt",
		},
		{
			name:    "known layers in a shape this build does not stage",
			layers:  []string{"fabric", "lvmVolumeGroup", "filesystem"},
			wantErr: "not a shape this build stages",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, _ := newStackedServer(t, newRecordingRunner())
			writeRecord(t, ns.stack, pvcTestHandle, tc.layers)

			plan, err := ns.teardownPlan(pvcTestHandle, "/staging", stagedContext())
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("the teardown accepted a plan it cannot release: %v", plan.Names())
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("the refusal does not name the layer it could not release: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("teardownPlan: %v", err)
			}
			if got := strings.Join(plan.Names(), " → "); got != tc.want {
				t.Errorf("the teardown walks %s, want %s", got, tc.want)
			}
		})
	}
}

// A volume staged by a version predating the stack has no record, and what that
// version built is fabric → filesystem. Nothing is restaged and no node is
// drained for it.
func TestTeardownPlanFallsBackToTheLegacyShape(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())

	plan, err := ns.teardownPlan(pvcTestHandle, "/staging", stagedContext())
	if err != nil {
		t.Fatalf("teardownPlan: %v", err)
	}
	if got := strings.Join(plan.Names(), " → "); got != plainShape {
		t.Errorf("a volume with no record is released as %s, want the legacy plan", got)
	}
}

// A record that cannot be read stops the teardown. That is not the same as an
// absent one: an absent record means the legacy plan, and a corrupt file is no
// evidence that a legacy stack was built.
func TestTeardownPlanRefusesAnUnreadableRecord(t *testing.T) {
	ns, recordDir := newStackedServer(t, newRecordingRunner())
	writeRecord(t, ns.stack, pvcTestHandle, []string{"fabric", "filesystem"})
	corruptRecord(t, recordDir)

	if _, err := ns.teardownPlan(pvcTestHandle, "/staging", stagedContext()); err == nil {
		t.Fatal("the teardown fell back to the legacy plan on a record it could not read")
	}
}

// A publish heals before it bind-mounts, because kubelet skips NodeStageVolume
// while the volume is still referenced on this node: a same-node pod
// replacement never stages again, so this is the only place a stack whose
// foundation went away gets repaired before a pod inherits it.
func TestPublishHealsBeforeBindMounting(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)
	parent := stashedAt(t, stagedContext())

	target := filepath.Join(t.TempDir(), "published")
	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: parent,
		TargetPath:        target,
		VolumeCapability:  mountCapability(),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	if !runner.called("heal") {
		t.Fatalf("the publish did not heal the stack first; it called %v", runner.calls)
	}
	if runner.called("up") {
		t.Fatal("the publish brought the stack up, which may format a device that already holds data")
	}
}

// An expansion grows the stack rather than one layer of it, so a volume whose
// capacity grew underneath several layers reaches its filesystem.
func TestExpandGrowsTheStack(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)
	parent := stashedAt(t, stagedContext())

	_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: parent,
		VolumePath:        filepath.Join(parent, pvcTestHandle),
		VolumeCapability:  mountCapability(),
	})
	if err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}

	if !runner.called("grow") {
		t.Fatalf("the expansion grew nothing; it called %v", runner.calls)
	}
	if got := strings.Join(runner.plans["grow"], " → "); got != plainShape {
		t.Errorf("the expansion walked %s, and a layer left out of the walk never gains the space", got)
	}
}

// A raw block volume's expansion walks a plan with nothing that can grow, which
// is the correct answer: the block device was already resized at the storage
// layer, and neither resize tool has anything to do with a device carrying no
// filesystem.
func TestExpandOfARawBlockVolumeGrowsNoFilesystem(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)
	parent := stashedAt(t, stagedContext())

	_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: parent,
		VolumePath:        filepath.Join(parent, pvcTestHandle),
		VolumeCapability:  blockCapability(),
	})
	if err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	if got := strings.Join(runner.plans["grow"], " → "); got != "fabric" {
		t.Errorf("the expansion walked %s, want the fabric alone", got)
	}
}

// writeRecord puts a stack record on the store, naming the layers given.
func writeRecord(t *testing.T, s *stack, handle string, names []string) {
	t.Helper()
	record := volstack.Record{Version: volstack.RecordVersion, VolumeHandle: handle}
	for _, layer := range names {
		entry := volstack.Entry{Layer: layer, Attempted: true}
		switch layer {
		case layerFilesystem:
			params, err := json.Marshal(map[string]string{"fsType": extFS})
			if err != nil {
				t.Fatalf("encode the filesystem parameters: %v", err)
			}
			entry.Params = params
		case layerLVMLogicalVolume:
			params, err := json.Marshal(layers.LVMLogicalVolumeParams{
				PoolName: vdoPoolName, Deduplication: true, Compression: true,
			})
			if err != nil {
				t.Fatalf("encode the logical-volume parameters: %v", err)
			}
			entry.Params = params
		}
		record.Plan = append(record.Plan, entry)
	}
	if err := s.store.Write(record); err != nil {
		t.Fatalf("write the stack record: %v", err)
	}
}

// corruptRecord truncates every record in dir to a prefix that does not decode,
// which is the shape a crash mid-write would leave if the write were not atomic.
func corruptRecord(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the record directory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("there is no record to corrupt")
	}
	for _, entry := range entries {
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), []byte(`{"version":1,"plan":[{"lay`), 0o600); err != nil {
			t.Fatalf("corrupt the record: %v", err)
		}
	}
}

// mountCapability and blockCapability are the two access types a volume is
// staged under. Every case here asks for ext4, and the cases that are about a
// disagreement put the other filesystem on the device rather than in the class.
func mountCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: extFS},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

func blockCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

// A bring-up that failed is tried once more after a fabric repair, and only
// when the repair actually tore something down.
//
// A connect can succeed at one layer of the NVMe object tree while the layer
// below it is unusable: a subsystem whose controllers are live and which
// exports no namespace at all satisfies every check a connect makes, produces
// no block device, and is retried by kubelet forever. Nothing changes between
// those retries unless something is torn down first, which is what the repair
// does and what makes a second attempt worth making.
func TestStageRetriesTheBringUpAfterAFabricRepair(t *testing.T) {
	runner := newRecordingRunner()
	runner.upErr = errors.New("fabric: wait for the namespace: timed out")
	ns, _ := newStackedServer(t, runner)

	repaired := false
	var diagnosed nvme.NamespaceID
	ns.repairFabric = func(_ context.Context, _ string, nsID nvme.NamespaceID) bool {
		repaired = true
		diagnosed = nsID
		runner.upErr = nil // the torn-down subsystem lets the next attach through
		return true
	}

	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCapability(),
		VolumeContext:     stagedContext(),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if !repaired {
		t.Fatal("the failed bring-up was not diagnosed, so kubelet would retry an attach nothing can change")
	}
	if got := strings.Count(strings.Join(runner.calls, " "), "up"); got != 2 {
		t.Errorf("the stack was brought up %d times, want one attempt and one retry after the repair", got)
	}
	// The repair acts on the namespace the plan named. A namespace id that
	// arrives here as anything else diagnoses a co-tenant of the subsystem, and
	// what it tears down then belongs to another volume.
	if want := namespaceID(stagedContext()); diagnosed != want {
		t.Errorf("the repair was pointed at namespace %d, want %d", diagnosed, want)
	}
}

// A repair that tore nothing down changes nothing, so the stage fails on the
// first answer rather than running the same attach twice.
func TestStageDoesNotRetryWhenNothingWasRepaired(t *testing.T) {
	runner := newRecordingRunner()
	runner.upErr = errors.New("fabric: attach refused")
	ns, _ := newStackedServer(t, runner)
	ns.repairFabric = func(context.Context, string, nvme.NamespaceID) bool { return false }

	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCapability(),
		VolumeContext:     stagedContext(),
	})
	if err == nil {
		t.Fatal("the stage reported success after a bring-up that failed")
	}
	if got := strings.Count(strings.Join(runner.calls, " "), "up"); got != 1 {
		t.Errorf("the stack was brought up %d times, want the one attempt", got)
	}
}

// A teardown identifies the volume's namespace from the record when the stashed
// context cannot, which is what a stash lost to a crash between the mount and
// the write leaves behind. The record names the subsystem and the namespace,
// and that is all a release needs.
func TestTeardownConnectionNamesTheSubsystemFromTheRecord(t *testing.T) {
	record := volstack.Record{
		Version:      volstack.RecordVersion,
		VolumeHandle: pvcTestHandle,
		Plan: []volstack.Entry{
			{Layer: layerFabric, Params: fabricParams(t, "nqn.recorded", 7)},
			{Layer: layerFilesystem},
		},
	}

	connection, err := teardownConnection(record, map[string]string{})
	if err != nil {
		t.Fatalf("teardownConnection: %v", err)
	}
	if connection.NQN != "nqn.recorded" || connection.NSID != 7 {
		t.Errorf("the teardown names %s namespace %d, want the recorded subsystem",
			connection.NQN, connection.NSID)
	}
}

// A teardown that can identify no namespace at all is refused rather than run.
// The fabric layer looks its device up by the identity it is given, and a
// selector that constrains nothing matches every namespace attached to the
// node: releasing on that would detach another volume.
func TestTeardownConnectionRefusesToIdentifyNothing(t *testing.T) {
	_, err := teardownConnection(volstack.Record{}, map[string]string{"targetType": "tcp"})
	if err == nil {
		t.Fatal("a teardown was built for a volume nothing identifies, and it would detach whatever it found")
	}
}

// fabricParams is one recorded fabric layer's parameters.
func fabricParams(t *testing.T, subsystemNQN string, nsID uint32) []byte {
	t.Helper()
	params, err := json.Marshal(layers.FabricParams{NQN: subsystemNQN, NSID: nsID})
	if err != nil {
		t.Fatalf("encode the fabric parameters: %v", err)
	}
	return params
}

// deletingPV is the test claim's PersistentVolume on its way out: the reclaim
// policy says the storage goes with it, and the claim behind it is terminating.
func deletingPV() *corev1.PersistentVolume {
	pv := boundPV()
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
	pv.Status.Phase = corev1.VolumeReleased
	return pv
}
