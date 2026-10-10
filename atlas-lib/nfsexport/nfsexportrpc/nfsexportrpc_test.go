// Tests for the export service's wire layer: that a spec survives the round
// trip intact, and that the two error shapes a caller has to tell apart arrive
// distinguishable on the far side.
//
// What the assembler does with a spec is tested in the nfsexport package. What is
// pinned here is only that the wire does not quietly change it, because a
// dropped client set would publish an export to nobody and a dropped fsid would
// publish one whose file handles no longer survive a move.

package nfsexportrpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/fiemap"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/ptr"
)

// recordingAssembler captures what reached the node.
type recordingAssembler struct {
	created  []nfsexport.Spec
	deleted  []nfsexport.Spec
	checked  []nfsexport.Spec
	err      error
	checkErr error
}

func (a *recordingAssembler) Create(_ context.Context, spec nfsexport.Spec) error {
	if a.err != nil {
		return a.err
	}
	a.created = append(a.created, spec)
	return nil
}

func (a *recordingAssembler) Delete(_ context.Context, spec nfsexport.Spec) error {
	if a.err != nil {
		return a.err
	}
	a.deleted = append(a.deleted, spec)
	return nil
}

func (a *recordingAssembler) Check(_ context.Context, spec nfsexport.Spec) error {
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

var fullSpec = nfsexport.Spec{
	VolumeUUID: "cb2f293c-6d6f-4687-ad13-eb81fbec7314",
	ClusterID:  "f0bb9077-78c4-4482-9ccf-a5693ce2df78",
	PoolID:     "9d016dd4-34d7-42f0-b549-52a5af2f1399",
	Path:       "/mnt/team-a-shared-3c81",
	FSID:       "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13",
	Clients:    []string{"192.168.10.21", "192.168.10.0/24"},
	Encrypted:  true,
	HostNQN:    "nqn.2023-02.io.simplyblock:host:5e1d7a0c-0f3b-4c1e-9a77-2b8d1f6e4c10",
	// Two paths, so order survives too, and timeouts both unset and zero. Zero
	// fails I/O at once and unset takes the connector's default, so a wire that
	// folds one into the other changes how a path fails.
	Connection: &lvol.Connection{
		NQN:  "nqn.2023-02.io.simplyblock:f0bb9077:lvol:cb2f293c-6d6f-4687-ad13-eb81fbec7314",
		NSID: 1,
		UUID: "cb2f293c-6d6f-4687-ad13-eb81fbec7314",
		Endpoints: []lvol.Endpoint{
			{
				Transport: "tcp", Address: "10.10.1.11", Port: 4420,
				NrIOQueues: 4, ReconnectDelaySec: 2, KeepAliveTMOSec: 5,
				CtrlLossTMOSec: ptr.To(60), FastIOFailTMOSec: ptr.To(0),
				HostIface: "eth0", TLS: true,
				DHCHAPSecret: "DHHC-1:00:host-secret:", DHCHAPCtrlSecret: "DHHC-1:00:ctrl-secret:",
			},
			{Transport: "tcp", Address: "10.10.1.12", Port: 4420},
		},
	},
}

// A spec without a resolved connection has to arrive without one, not with an
// empty one: the host resolves the connection itself when none is supplied,
// and an empty one would be taken as the answer.
func TestASpecWithoutAConnectionArrivesWithoutOne(t *testing.T) {
	assembler := &recordingAssembler{}
	client := serve(t, assembler)
	spec := fullSpec
	spec.Connection = nil
	spec.HostNQN = ""

	if err := client.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := assembler.created[0]; got.Connection != nil || got.HostNQN != "" {
		t.Errorf("connection = %+v, hostNQN = %q, want none", got.Connection, got.HostNQN)
	}
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
	client := serve(t, &recordingAssembler{err: nfsexport.ErrInvalidSpec})

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
	if len(caps) != 1 || caps[0] != "atlas.nfsexport.v1.ExportService" {
		t.Errorf("capabilities = %v, want the ExportService name", caps)
	}
}

// fakeExtents answers FileExtents with a fixed map and records what it was asked.
type fakeExtents struct {
	asked []string
	err   error
}

func (f *fakeExtents) FileExtents(
	_ context.Context, exportPath, file string, offset, length uint64,
) (nfsexport.FileExtents, error) {
	f.asked = append(f.asked, exportPath+"|"+file)
	if f.err != nil {
		return nfsexport.FileExtents{}, f.err
	}
	return nfsexport.FileExtents{Size: 3 * 1 << 20, Pieces: []fiemap.Piece{
		{Offset: offset, Length: length / 2, Kind: fiemap.Written, Physical: 4096},
		{Offset: offset + length/2, Length: length - length/2, Kind: fiemap.Hole},
	}}, nil
}

func serveWith(t *testing.T, opts ...Option) *Client {
	t.Helper()
	srv, err := NewServer(&recordingAssembler{}, opts...)
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

// The map crosses the wire intact: size, and every piece's kind and offsets.
func TestFileExtentsRoundTripsTheMap(t *testing.T) {
	reader := &fakeExtents{}
	client := serveWith(t, WithExtentReader(reader))

	got, err := client.FileExtents(context.Background(), "/var/lib/simplyblock/exports/a", "f.r1", 1<<20, 8192)
	if err != nil {
		t.Fatalf("FileExtents: %v", err)
	}
	want := nfsexport.FileExtents{Size: 3 * 1 << 20, Pieces: []fiemap.Piece{
		{Offset: 1 << 20, Length: 4096, Kind: fiemap.Written, Physical: 4096},
		{Offset: 1<<20 + 4096, Length: 4096, Kind: fiemap.Hole},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FileExtents = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(reader.asked, []string{"/var/lib/simplyblock/exports/a|f.r1"}) {
		t.Errorf("reader asked %v", reader.asked)
	}
}

// A server built without a reader says so, rather than answering with an empty map
// that would read as "nothing there."
func TestFileExtentsWithoutAReaderIsUnimplemented(t *testing.T) {
	client := serveWith(t)
	_, err := client.FileExtents(context.Background(), "/x", "f", 0, 4096)
	if err == nil || !strings.Contains(err.Error(), "Unimplemented") {
		t.Errorf("FileExtents without a reader = %v, want Unimplemented", err)
	}
}

// A refused path is an error carrying its reason, never an empty map that would
// read as "nothing there."
func TestARefusedFileArrivesWithItsReason(t *testing.T) {
	client := serveWith(t, WithExtentReader(&fakeExtents{
		err: fmt.Errorf("%w: /etc is not an export", nfsexport.ErrInvalidSpec)}))
	got, err := client.FileExtents(context.Background(), "/etc", "passwd", 0, 4096)
	if err == nil || !strings.Contains(err.Error(), "/etc is not an export") {
		t.Errorf("FileExtents of a refused path = %+v, %v; want the reason", got, err)
	}
}
