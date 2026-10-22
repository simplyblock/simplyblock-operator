// CreateVolume through a class that names a supercluster
// (design-superclusters.md §7.2, §9.2): the driver asks the operator which
// member, creates the volume there, and maps the operator's answer to what the
// provisioner sees.

package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/placement/placementrpc"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// testAggregateID is a supercluster's identifier, which no entry of the
// driver's secret carries.
const testAggregateID = "0d6f1b2e-1f0a-4c71-9a0e-5b7d2c3e4f50"

// fakePlacer answers Place with a member or an error, and remembers the
// requests.
type fakePlacer struct {
	mu       sync.Mutex
	requests []placementrpc.Request
	member   string
	err      error
}

func (p *fakePlacer) Place(_ context.Context, req placementrpc.Request) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	return p.member, p.err
}

func (p *fakePlacer) asked() []placementrpc.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]placementrpc.Request(nil), p.requests...)
}

// aggregateRequest is a CreateVolume of volume pvc-1 through a class naming
// the supercluster, for the claim app/data.
func aggregateRequest() *csi.CreateVolumeRequest {
	req := basicCreateVolumeRequest("pvc-1")
	req.Parameters[csicommon.ParamClusterID] = testAggregateID
	req.Parameters[csicommon.CSIStorageNameKey] = "data"
	req.Parameters[csicommon.CSIStorageNamespaceKey] = "app"
	req.AccessibilityRequirements = &csi.TopologyRequirement{
		Preferred: []*csi.Topology{{Segments: map[string]string{"topology.kubernetes.io/region": "eu-west-1"}}},
	}
	return req
}

func createdVolumes(m *mockSBCLI) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.volumes)
}

// U-112, U-61, U-59: a class whose cluster_id names no cluster in the secret
// asks the operator, the volume is created once in the member answered, and the
// handle names that member.
func TestAClassNamingASuperclusterAsksTheOperator(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	cs.kubeClient = k8sfake.NewClientset(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data", Namespace: "app",
	}})
	placer := &fakePlacer{member: sanityClusterID}
	cs.SetPlacer(placer)

	resp, err := cs.CreateVolume(context.Background(), aggregateRequest())

	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got := resp.GetVolume().GetVolumeId(); !strings.HasPrefix(got, sanityClusterID+":") {
		t.Errorf("handle = %q, want it to begin with the member %s", got, sanityClusterID)
	}
	if n := createdVolumes(mock); n != 1 {
		t.Errorf("volumes created = %d, want 1", n)
	}
	asked := placer.asked()
	if len(asked) != 1 {
		t.Fatalf("the operator was asked %d times, want 1", len(asked))
	}
	got := asked[0]
	if got.AggregateID != testAggregateID || got.PoolName != sanityPoolName || got.VolumeName != "pvc-1" ||
		got.SizeBytes != 1<<30 || got.ClaimName != "data" || got.ClaimNamespace != "app" {
		t.Errorf("request = %+v", got)
	}
	if len(got.Preferred) != 1 || got.Preferred[0]["topology.kubernetes.io/region"] != "eu-west-1" {
		t.Errorf("preferred topology = %v, want the request's", got.Preferred)
	}
	if got := resp.GetVolume().GetVolumeContext()[csicommon.ParamClusterID]; got != sanityClusterID {
		t.Errorf("volume context cluster_id = %q, want the member", got)
	}
}

// U-149: a class naming a member's UUID never asks the operator.
func TestAClassNamingAMemberNeverAsks(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	placer := &fakePlacer{member: "never"}
	cs.SetPlacer(placer)

	if _, err := cs.CreateVolume(context.Background(), basicCreateVolumeRequest("pvc-1")); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got := placer.asked(); len(got) != 0 {
		t.Errorf("the operator was asked %d times, want none", len(got))
	}
}

// U-113: an identifier the operator knows no supercluster for is
// InvalidArgument naming it, as an unknown cluster is.
func TestAnUnknownAggregateIsInvalidArgument(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	cs.SetPlacer(&fakePlacer{err: status.Error(codes.NotFound, "no supercluster has the identifier")})

	_, err := cs.CreateVolume(context.Background(), aggregateRequest())

	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), testAggregateID) {
		t.Errorf("CreateVolume = %v, want InvalidArgument naming %s", err, testAggregateID)
	}
}

// U-148: no link to the operator fails with Unavailable, which the provisioner
// retries, and nothing is created.
func TestNoLinkIsUnavailable(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)

	_, err := cs.CreateVolume(context.Background(), aggregateRequest())

	if status.Code(err) != codes.Unavailable {
		t.Errorf("CreateVolume without a link = %v, want Unavailable", err)
	}
	if n := createdVolumes(mock); n != 0 {
		t.Errorf("volumes created = %d, want 0", n)
	}
}

// The operator's other answers reach the provisioner with their code.
func TestTheOperatorsRefusalsPassThrough(t *testing.T) {
	for _, code := range []codes.Code{codes.ResourceExhausted, codes.FailedPrecondition, codes.Unavailable} {
		mock := newMockSBCLI()
		cs := newTestControllerServer(t, mock)
		cs.SetPlacer(&fakePlacer{err: status.Error(code, "member prod-a is full")})

		_, err := cs.CreateVolume(context.Background(), aggregateRequest())

		if status.Code(err) != code || !strings.Contains(err.Error(), "prod-a") {
			t.Errorf("CreateVolume = %v, want %s with the operator's message", err, code)
		}
		if n := createdVolumes(mock); n != 0 {
			t.Errorf("volumes created after %s = %d, want 0", code, n)
		}
		mock.Close()
	}
}

// U-165, the driver's half: the claim's consistency-group label is sent.
func TestTheClaimsGroupIsSent(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	cs.kubeClient = k8sfake.NewClientset(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data", Namespace: "app", Labels: map[string]string{consistencyGroupLabel: "orders"},
	}})
	placer := &fakePlacer{err: status.Error(codes.ResourceExhausted, "stop here")}
	cs.SetPlacer(placer)

	_, _ = cs.CreateVolume(context.Background(), aggregateRequest())

	if asked := placer.asked(); len(asked) != 1 || asked[0].ConsistencyGroup != "orders" {
		t.Errorf("requests = %+v, want the group orders", asked)
	}
}

// U-62 and U-63, the driver's half: a clone through a supercluster class sends
// its source's cluster, so the operator answers that member, and the
// operator's refusal of a source outside the supercluster is the driver's.
func TestACloneSendsItsSourcesCluster(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	placer := &fakePlacer{err: status.Error(codes.InvalidArgument, "not a member")}
	cs.SetPlacer(placer)
	req := aggregateRequest()
	req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{
			SnapshotId: sanityClusterID + ":" + sanityPoolUUID + ":22222222-2222-2222-2222-222222222222",
		},
	}}

	_, err := cs.CreateVolume(context.Background(), req)

	if asked := placer.asked(); len(asked) != 1 || asked[0].SourceClusterID != sanityClusterID {
		t.Errorf("requests = %+v, want the source cluster %s", asked, sanityClusterID)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateVolume = %v, want the operator's InvalidArgument", err)
	}
}

// U-111: a class carrying a zone map routes by the map and never reaches the
// operator, whatever its clusters are.
func TestAClassWithAZoneMapNeverAsks(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	placer := &fakePlacer{member: "never"}
	cs.SetPlacer(placer)
	req := basicCreateVolumeRequest("pvc-1")
	delete(req.Parameters, csicommon.ParamClusterID)
	req.Parameters[paramZoneClusterMap] = `{"zone-a":"` + sanityClusterID + `"}`
	req.AccessibilityRequirements = &csi.TopologyRequirement{
		Preferred: []*csi.Topology{{Segments: map[string]string{csicommon.TopologyKeyZoneStable: "zone-a"}}},
	}

	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got := placer.asked(); len(got) != 0 {
		t.Errorf("the operator was asked %d times, want none", len(got))
	}
}

// recordingExports keeps the export records asked for.
type recordingExports struct {
	specs []ExportSpec
}

func (e *recordingExports) EnsureExport(_ context.Context, _ string, spec ExportSpec) (ExportRecord, error) {
	e.specs = append(e.specs, spec)
	return ExportRecord{Phase: "Ready", ServiceAddress: "10.0.0.9", ExportPath: spec.ExportPath}, nil
}

func (e *recordingExports) DeleteExport(context.Context, string) (bool, error) { return true, nil }

func (e *recordingExports) SetExportSize(context.Context, string, int64) (bool, error) { return true, nil }

// A pNFS claim through a supercluster class is exported from the member its
// volume was placed in: the export names that member's volume, so its metadata
// server serves a volume of one member, and nothing stripes across members.
func TestAPNFSClaimIsExportedFromItsMember(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	cs.kubeClient = k8sfake.NewClientset(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data", Namespace: "app",
	}})
	exports := &recordingExports{}
	cs.exports = exports
	cs.SetPlacer(&fakePlacer{member: sanityClusterID})
	req := aggregateRequest()
	req.VolumeCapabilities[0].AccessType = &csi.VolumeCapability_Mount{
		Mount: &csi.VolumeCapability_MountVolume{FsType: pnfsFSType},
	}

	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	if len(exports.specs) != 1 || !strings.HasPrefix(exports.specs[0].VolumeRef, sanityClusterID+":") {
		t.Errorf("exports = %+v, want one of the member's volume", exports.specs)
	}
}
