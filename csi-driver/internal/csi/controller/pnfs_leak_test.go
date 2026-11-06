// SFAM-2845: a pNFS claim that can never be served must leave nothing behind.
//
// A claim that never binds never gets a PV, so DeleteVolume is never called
// for it. Whatever CreateVolume allocates for such a claim has to be refused
// before it exists, or rolled back before a final error is returned.

package controller

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

func pnfsCreateVolumeRequest(name string) *csi.CreateVolumeRequest {
	req := basicCreateVolumeRequest(name)
	req.VolumeCapabilities = []*csi.VolumeCapability{
		mountCapMode(pnfsFSType, csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	}
	req.Parameters[csicommon.CSIStorageNamespaceKey] = "team-a"
	req.Parameters[csicommon.CSIStorageNameKey] = "shared"
	return req
}

// newPNFSTestServer wires the claim's PVC too: CreateVolume reads it when the
// provisioner passes the claim's name and namespace.
func newPNFSTestServer(t *testing.T, mock *mockSBCLI, registry ExportRegistry) *Server {
	t.Helper()
	cs := newTestControllerServer(t, mock)
	cs.kubeClient = fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"},
	})
	cs.exports = registry
	return cs
}

func mockVolumeCount(mock *mockSBCLI) int {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	return len(mock.volumes)
}

// With no metadata server the claim is refused before anything is allocated:
// no lvol, no export.
func TestPNFSWithoutMetadataServerAllocatesNothing(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	registry := &fakeRegistry{noMDS: true}
	cs := newPNFSTestServer(t, mock, registry)

	_, err := cs.CreateVolume(context.Background(), pnfsCreateVolumeRequest("pvc-no-mds"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s (%v), want FailedPrecondition", status.Code(err), err)
	}
	if got := mockVolumeCount(mock); got != 0 {
		t.Errorf("%d volumes allocated for a claim that cannot be served, want 0", got)
	}
	if len(registry.ensured) != 0 {
		t.Errorf("exports created for a claim that cannot be served: %v", registry.ensured)
	}
}

// A degraded export is a final refusal, so the volume behind it is rolled back
// first. While the operator still finalizes the export the answer stays
// Unavailable, and the volume stays until the export is gone.
func TestDegradedPNFSExportRollsBackItsVolume(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	registry := &fakeRegistry{record: ExportRecord{Phase: "Degraded", Message: "assembly failed"}}
	cs := newPNFSTestServer(t, mock, registry)
	req := pnfsCreateVolumeRequest("pvc-degraded")

	// The export is still being torn down on its host.
	_, err := cs.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("mid-rollback code = %s (%v), want Unavailable", status.Code(err), err)
	}
	if len(registry.deleted) != 1 {
		t.Fatalf("export deletions = %v, want one", registry.deleted)
	}
	if got := mockVolumeCount(mock); got != 1 {
		t.Fatalf("volume deleted before its export was gone: %d volumes, want 1", got)
	}

	// The export is gone: the retry finishes the rollback and refuses for good.
	registry.stillGone = true
	_, err = cs.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("final code = %s (%v), want FailedPrecondition", status.Code(err), err)
	}
	if got := mockVolumeCount(mock); got != 0 {
		t.Errorf("%d volumes left behind a degraded export, want 0", got)
	}
}

// An export already being deleted will never become Ready; the same rollback
// applies.
func TestDeletingPNFSExportRollsBackItsVolume(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newPNFSTestServer(t, mock, &fakeRegistry{record: ExportRecord{Phase: "Pending", Deleting: true}, stillGone: true})

	_, err := cs.CreateVolume(context.Background(), pnfsCreateVolumeRequest("pvc-deleting"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s (%v), want FailedPrecondition", status.Code(err), err)
	}
	if got := mockVolumeCount(mock); got != 0 {
		t.Errorf("%d volumes left behind a deleted export, want 0", got)
	}
}

// A pending export with a metadata server configured is still in progress:
// nothing is rolled back.
func TestPendingPNFSExportKeepsItsVolume(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	registry := &fakeRegistry{record: ExportRecord{Phase: "Pending"}}
	cs := newPNFSTestServer(t, mock, registry)

	_, err := cs.CreateVolume(context.Background(), pnfsCreateVolumeRequest("pvc-pending"))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %s (%v), want Unavailable", status.Code(err), err)
	}
	if len(registry.deleted) != 0 || mockVolumeCount(mock) != 1 {
		t.Errorf("a pending export was rolled back: deleted=%v volumes=%d",
			registry.deleted, mockVolumeCount(mock))
	}
}
