// Tests for the export service's wire layer: that a spec survives the round
// trip intact, and that the two error shapes a caller has to tell apart arrive
// distinguishable on the far side.
//
// What the assembler does with a spec is tested in the export package. What is
// pinned here is only that the wire does not quietly change it, because a
// dropped client set would publish an export to nobody and a dropped fsid would
// publish one whose file handles no longer survive a move.

package exportrpc

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/export"
)

// recordingAssembler captures what reached the node.
type recordingAssembler struct {
	created  []export.Spec
	deleted  []export.Spec
	checked  []export.Spec
	err      error
	checkErr error
}

func (a *recordingAssembler) Create(_ context.Context, spec export.Spec) error {
	if a.err != nil {
		return a.err
	}
	a.created = append(a.created, spec)
	return nil
}

func (a *recordingAssembler) Delete(_ context.Context, spec export.Spec) error {
	if a.err != nil {
		return a.err
	}
	a.deleted = append(a.deleted, spec)
	return nil
}

func (a *recordingAssembler) Check(_ context.Context, spec export.Spec) error {
	a.checked = append(a.checked, spec)
	return a.checkErr
}

// serve starts the service on an in-memory listener and returns a client for it.
func serve(t *testing.T, assembler Assembler) *Client {
	t.Helper()
	srv, err := NewServer(assembler)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	lis := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	srv.Register(grpcServer)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return Remote(conn)
}

var fullSpec = export.Spec{
	VolumeUUID: "cb2f293c-6d6f-4687-ad13-eb81fbec7314",
	ClusterID:  "f0bb9077-78c4-4482-9ccf-a5693ce2df78",
	PoolID:     "9d016dd4-34d7-42f0-b549-52a5af2f1399",
	Path:       "/mnt/team-a-shared-3c81",
	FSID:       "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13",
	Clients:    []string{"192.168.10.21", "192.168.10.0/24"},
	Encrypted:  true,
}

// Regression: 2026-09-23-pnfs-encryption-dropped -- every field crosses. A
// spec that loses its encryption bit can mistake ciphertext for a formatted
// plaintext filesystem. A spec that loses its client set publishes to nobody, one
// that loses its fsid publishes an export whose handles do not survive a move,
// and one that loses its cluster or pool cannot attach the namespace at all, so
// none of them can be allowed to go missing quietly.
func TestCreateRoundTripsTheWholeSpec(t *testing.T) {
	assembler := &recordingAssembler{}
	client := serve(t, assembler)

	if err := client.Create(context.Background(), fullSpec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(assembler.created) != 1 {
		t.Fatalf("assembler saw %d creates, want 1", len(assembler.created))
	}
	if got := assembler.created[0]; !reflect.DeepEqual(got, fullSpec) {
		t.Errorf("spec did not survive the round trip:\n got %+v\nwant %+v", got, fullSpec)
	}
}

func TestDeleteRoundTripsTheWholeSpec(t *testing.T) {
	assembler := &recordingAssembler{}
	client := serve(t, assembler)

	if err := client.Delete(context.Background(), fullSpec); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(assembler.deleted) != 1 {
		t.Fatalf("assembler saw %d deletes, want 1", len(assembler.deleted))
	}
	if got := assembler.deleted[0]; !reflect.DeepEqual(got, fullSpec) {
		t.Errorf("spec did not survive the round trip:\n got %+v\nwant %+v", got, fullSpec)
	}
}

// A healthy export round trips as Check returning nil, not as a response the
// caller has to unpack.
func TestCheckRoundTripsAHealthyExport(t *testing.T) {
	assembler := &recordingAssembler{}
	client := serve(t, assembler)

	if err := client.Check(context.Background(), fullSpec); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(assembler.checked) != 1 || !reflect.DeepEqual(assembler.checked[0], fullSpec) {
		t.Errorf("checked = %+v, want one call carrying the whole spec", assembler.checked)
	}
}

// An unhealthy export is not a transport failure: it is the call succeeding
// and reporting a clear, negative answer, which arrives back to the caller as
// an error naming the reason -- the one signal a caller that only wants
// "fine, or not" needs, per exportrpc.Assembler's contract.
func TestCheckReportsAnUnhealthyExportAsAnError(t *testing.T) {
	assembler := &recordingAssembler{checkErr: errors.New("export: not mounted: exit status 32")}
	client := serve(t, assembler)

	err := client.Check(context.Background(), fullSpec)
	if err == nil {
		t.Fatal("Check passed over an unhealthy export")
	}
	if !strings.Contains(err.Error(), "not mounted") {
		t.Errorf("error = %q, want it to carry the node's reason", err.Error())
	}
}

// A refused spec has to arrive as a refusal, not as a transport failure: the
// caller retries one and gives up on the other.
func TestInvalidSpecArrivesAsInvalid(t *testing.T) {
	client := serve(t, &recordingAssembler{err: export.ErrInvalidSpec})

	err := client.Create(context.Background(), fullSpec)
	if err == nil {
		t.Fatal("a refused spec returned no error")
	}
	if errors.Is(err, errs.ErrNotFound) {
		t.Errorf("a refused spec arrived as not-found: %v", err)
	}
}

// A device that is not there is not-found on the far side too, which is what
// lets the caller wait for it rather than give up on the export.
func TestMissingDeviceArrivesAsNotFound(t *testing.T) {
	client := serve(t, &recordingAssembler{err: errs.ErrNotFound})

	err := client.Create(context.Background(), fullSpec)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("err = %v, want it to carry errs.ErrNotFound", err)
	}
}

// A nil assembler is refused at construction. A node that advertised the
// capability and cannot honor it is worse than one that never advertised it,
// because the operator would bind an export to it.
func TestNewServerRefusesANilAssembler(t *testing.T) {
	if _, err := NewServer(nil); err == nil {
		t.Fatal("NewServer(nil) succeeded")
	}
}

// The capability name is what the operator matches on to tell a node that can
// serve exports from one that is merely connected, so it is pinned rather than
// left to drift with a rename.
func TestCapabilityNameIsStable(t *testing.T) {
	caps := Capabilities()
	if len(caps) != 1 || caps[0] != "atlas.export.v1.ExportService" {
		t.Errorf("capabilities = %v, want the ExportService name", caps)
	}
}
