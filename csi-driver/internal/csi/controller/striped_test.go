// What a striped volume has to guarantee on the controller side: one member per
// storage node, every operation across every member, and nothing left behind by
// an operation that could not finish.

package controller

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/csi-addons/spec/lib/go/replication"
	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

const (
	stripeNodeA = "a0000000-0000-4000-8000-00000000000a"
	stripeNodeB = "b0000000-0000-4000-8000-00000000000b"
	stripeNodeC = "c0000000-0000-4000-8000-00000000000c"
	stripeNodeD = "d0000000-0000-4000-8000-00000000000d"
)

// storageNodes is a cluster's node list, every node online unless named in
// offline.
func storageNodes(ids []string, offline ...string) []atlascp.StorageNode {
	nodes := make([]atlascp.StorageNode, 0, len(ids))
	for _, id := range ids {
		state := lvolOnline
		if slices.Contains(offline, id) {
			state = "offline"
		}
		nodes = append(nodes, atlascp.StorageNode{ID: id, ClusterID: sanityClusterID, Status: state})
	}
	return nodes
}

// stripedServer is a controller over the mock whose cluster has the given
// storage nodes.
func stripedServer(t *testing.T, mock *mockSBCLI, nodes []atlascp.StorageNode) *Server {
	t.Helper()
	cs := newTestControllerServer(t, mock)
	cs.storageNodes = func(context.Context, string) ([]atlascp.StorageNode, error) { return nodes, nil }
	// Attached nowhere unless a test says otherwise, so nothing needs a freeze.
	cs.attachedNodes = func(context.Context, string) ([]string, error) { return nil, nil }
	return cs
}

// stripedRequest asks for a volume of capacity bytes striped width wide.
func stripedRequest(name, width string, capacity int64) *csi.CreateVolumeRequest {
	req := basicCreateVolumeRequest(name)
	req.CapacityRange.RequiredBytes = capacity
	req.Parameters["stripe_width"] = width
	return req
}

// volumesNamed is the mock's volumes by name.
func volumesNamed(mock *mockSBCLI) map[string]*mockVolume {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	out := map[string]*mockVolume{}
	for _, v := range mock.volumes {
		out[v.Name] = v
	}
	return out
}

// createStriped creates a striped volume and fails the test if it cannot.
func createStriped(t *testing.T, cs *Server, req *csi.CreateVolumeRequest) *csi.Volume {
	t.Helper()
	resp, err := cs.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	return resp.GetVolume()
}

func TestParseStripeSpec(t *testing.T) {
	ok := []struct {
		params map[string]string
		want   stripeSpec
	}{
		{map[string]string{}, stripeSpec{width: 1}},
		{map[string]string{"stripe_width": ""}, stripeSpec{width: 1}},
		{map[string]string{"stripe_width": "0"}, stripeSpec{width: 1}},
		{map[string]string{"stripe_width": "1"}, stripeSpec{width: 1}},
		// A class that never asked for striping has stripe_size ignored, so it
		// provisions exactly as it did before.
		{map[string]string{"stripe_width": "1", "stripe_size": "garbage"}, stripeSpec{width: 1}},
		{map[string]string{"stripe_width": "3"}, stripeSpec{width: 3, chunkBytes: 64 << 10}},
		{map[string]string{"stripe_width": " 4 ", "stripe_size": "128Ki"}, stripeSpec{width: 4, chunkBytes: 128 << 10}},
		{map[string]string{"stripe_width": "2", "stripe_size": "4096"}, stripeSpec{width: 2, chunkBytes: 4096}},
		{map[string]string{"stripe_width": "2", "stripe_size": "1Mi"}, stripeSpec{width: 2, chunkBytes: 1 << 20}},
		{map[string]string{"stripe_width": "128"}, stripeSpec{width: 128, chunkBytes: 64 << 10}},
	}
	for _, tc := range ok {
		got, err := parseStripeSpec(tc.params)
		if err != nil || got != tc.want {
			t.Errorf("parseStripeSpec(%v) = %+v, %v; want %+v", tc.params, got, err, tc.want)
		}
	}

	bad := []map[string]string{
		{"stripe_width": "two"},
		{"stripe_width": "-2"},
		{"stripe_width": "2.5"},
		{"stripe_width": "129"},
		{"stripe_width": "2", "stripe_size": "2Ki"},
		{"stripe_width": "2", "stripe_size": "2Mi"},
		{"stripe_width": "2", "stripe_size": "96Ki"},
		{"stripe_width": "2", "stripe_size": "64k"}, // 64000 bytes, not a power of two
		{"stripe_width": "2", "stripe_size": "lots"},
	}
	for _, params := range bad {
		if got, err := parseStripeSpec(params); status.Code(err) != codes.InvalidArgument {
			t.Errorf("parseStripeSpec(%v) = %+v, %v; want InvalidArgument", params, got, err)
		}
	}
}

func TestStripeMemberBytesRoundsEachShareUpToAGiB(t *testing.T) {
	cases := []struct {
		capacity int64
		width    int
		want     int64
	}{
		{10 * gib, 2, 5 * gib},
		{10 * gib, 3, 4 * gib},
		{1, 4, gib},
		{3*gib + 1, 3, 2 * gib},
	}
	for _, tc := range cases {
		got := stripeMemberBytes(tc.capacity, tc.width)
		if got != tc.want {
			t.Errorf("stripeMemberBytes(%d, %d) = %d, want %d", tc.capacity, tc.width, got, tc.want)
		}
		if got*int64(tc.width) < tc.capacity {
			t.Errorf("stripeMemberBytes(%d, %d) adds up to less than was asked for", tc.capacity, tc.width)
		}
	}
}

func TestPickStripeHosts(t *testing.T) {
	nodes := storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC, stripeNodeD})

	t.Run("one node per member, the same on a retry", func(t *testing.T) {
		first, err := pickStripeHosts(nodes, 3, "pvc-1", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		assertDistinctHosts(t, first, 3)
		again, _ := pickStripeHosts(nodes, 3, "pvc-1", "", nil)
		if !slices.Equal(first, again) {
			t.Errorf("a retry chose %v after %v; the choice has to be deterministic", again, first)
		}
	})

	t.Run("a pin goes to the first member", func(t *testing.T) {
		hosts, err := pickStripeHosts(nodes, 3, "pvc-1", stripeNodeD, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertDistinctHosts(t, hosts, 3)
		if hosts[0] != stripeNodeD {
			t.Errorf("hosts = %v, want the pinned node first", hosts)
		}
	})

	t.Run("existing members keep their nodes and nobody joins them", func(t *testing.T) {
		hosts, err := pickStripeHosts(nodes, 4, "pvc-1", "", map[int]string{1: stripeNodeA, 3: stripeNodeB})
		if err != nil {
			t.Fatal(err)
		}
		assertDistinctHosts(t, hosts, 4)
		if hosts[1] != stripeNodeA || hosts[3] != stripeNodeB {
			t.Errorf("hosts = %v, want members 1 and 3 left where they are", hosts)
		}
	})

	t.Run("consecutive members land in different failure domains", func(t *testing.T) {
		domains := []atlascp.StorageNode{
			{ID: stripeNodeA, Status: lvolOnline, FailureDomain: 1},
			{ID: stripeNodeB, Status: lvolOnline, FailureDomain: 1},
			{ID: stripeNodeC, Status: lvolOnline, FailureDomain: 2},
			{ID: stripeNodeD, Status: lvolOnline, FailureDomain: 2},
		}
		domainOf := map[string]int{stripeNodeA: 1, stripeNodeB: 1, stripeNodeC: 2, stripeNodeD: 2}
		for _, name := range []string{"pvc-1", "pvc-2", "pvc-3", "pvc-4"} {
			hosts, err := pickStripeHosts(domains, 2, name, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if domainOf[hosts[0]] == domainOf[hosts[1]] {
				t.Errorf("%s: hosts %v are both in failure domain %d", name, hosts, domainOf[hosts[0]])
			}
		}
	})

	refusals := []struct {
		name   string
		width  int
		pinned string
		taken  map[int]string
	}{
		{"wider than the cluster", 5, "", nil},
		{"pinned to a node that is not online", 2, "e0000000-0000-4000-8000-00000000000e", nil},
		{"pinned to a node an existing member holds", 3, stripeNodeA, map[int]string{2: stripeNodeA}},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			hosts, err := pickStripeHosts(nodes, tc.width, "pvc-1", tc.pinned, tc.taken)
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("pickStripeHosts = %v, %v; want InvalidArgument", hosts, err)
			}
		})
	}
}

func assertDistinctHosts(t *testing.T, hosts []string, width int) {
	t.Helper()
	if len(hosts) != width {
		t.Fatalf("hosts = %v, want %d", hosts, width)
	}
	seen := map[string]bool{}
	for i, h := range hosts {
		if h == "" || seen[h] {
			t.Fatalf("hosts = %v: member %d has no node of its own", hosts, i)
		}
		seen[h] = true
	}
}

// TestCreateStripedVolume holds a striped create to its shape: width members of
// an equal share, named by the rule, each on its own node, behind an id that
// carries the width and a context that lists them in order.
func TestCreateStripedVolume(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC, stripeNodeD}))

	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 10*gib))

	handle, width, ok := lvol.ParseStripedHandle(lvol.VolumeHandle(vol.GetVolumeId()))
	if !ok || width != 3 || !strings.HasSuffix(vol.GetVolumeId(), ":s3") {
		t.Fatalf("volume id %q is not a striped handle of width 3", vol.GetVolumeId())
	}
	if vol.GetCapacityBytes() != 12*gib {
		t.Errorf("capacity = %d, want 3 members of 4GiB", vol.GetCapacityBytes())
	}

	byName := volumesNamed(mock)
	if len(byName) != 3 {
		t.Fatalf("the control plane holds %d volumes, want 3: %v", len(byName), byName)
	}
	members, hosts := make([]string, 0, 3), make([]string, 0, 3)
	for i := range 3 {
		m, ok := byName[stripeMemberName("pvc-striped", i)]
		if !ok {
			t.Fatalf("member %d (%s) was not created", i, stripeMemberName("pvc-striped", i))
			return
		}
		if m.Size != 4*gib {
			t.Errorf("member %d is %d bytes, want 4GiB", i, m.Size)
		}
		members = append(members, m.UUID)
		hosts = append(hosts, m.HostID)
	}
	assertDistinctHosts(t, hosts, 3)
	if members[0] != handle.VolumeID {
		t.Errorf("the id names %s, want the first member %s", handle.VolumeID, members[0])
	}

	vc := vol.GetVolumeContext()
	if vc[csicommon.CtxStripeWidth] != "3" || vc[csicommon.CtxStripeMembers] != strings.Join(members, ",") ||
		vc[csicommon.CtxStripeChunkBytes] != strconv.Itoa(64<<10) {
		t.Errorf("volume context = %v, want the width, the members in order, and the chunk", vc)
	}
	if _, ok := vc["xfs_su"]; ok {
		t.Errorf("the controller wrote xfs_su %q; the node's filesystem layer states the stripe's geometry", vc["xfs_su"])
	}
}

// TestAStripeWidthOfOneIsAnOrdinaryVolume pins the opt-in: a class saying 1 is
// provisioned exactly as one saying nothing.
func TestAStripeWidthOfOneIsAnOrdinaryVolume(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, nil)

	vol := createStriped(t, cs, stripedRequest("pvc-plain", "1", gib))
	if _, err := csicommon.ParseVolumeHandle(vol.GetVolumeId()); err != nil || strings.Count(vol.GetVolumeId(), ":") != 2 {
		t.Errorf("volume id %q is not a plain three-segment handle", vol.GetVolumeId())
	}
	if len(volumesNamed(mock)) != 1 {
		t.Errorf("an ordinary volume made %d logical volumes", len(volumesNamed(mock)))
	}
	if _, ok := vol.GetVolumeContext()[csicommon.CtxStripeWidth]; ok {
		t.Errorf("an ordinary volume's context carries %s", csicommon.CtxStripeWidth)
	}
}

func TestCreateStripedVolumeIsIdempotent(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}))

	first := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))
	again := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))
	if first.GetVolumeId() != again.GetVolumeId() {
		t.Errorf("a retry answered %s after %s", again.GetVolumeId(), first.GetVolumeId())
	}
	if n := len(volumesNamed(mock)); n != 3 {
		t.Errorf("after a retry the control plane holds %d volumes, want 3", n)
	}
}

// TestCreateStripedVolumeCompletesWhatAnEarlierAttemptLeft is the retry after a
// crash: the members already there are kept where they are, and only the
// missing ones are made, on nodes none of the kept ones occupy.
func TestCreateStripedVolumeCompletesWhatAnEarlierAttemptLeft(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}))

	// Whichever node the plan would have given the first member is the one
	// the leftover is on, so a plan that ignored it would collide.
	planned, err := pickStripeHosts(
		storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}), 3, "pvc-striped", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	const leftover = "11111111-0000-4000-8000-000000000001"
	mock.volumes[leftover] = &mockVolume{UUID: leftover, Name: "pvc-striped-s1", Size: gib, HostID: planned[0]}

	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))
	byName := volumesNamed(mock)
	if len(byName) != 3 || byName["pvc-striped-s1"].UUID != leftover {
		t.Fatalf("volumes = %v, want the leftover kept as member 1 and two more", byName)
	}
	hosts := []string{byName["pvc-striped"].HostID, byName["pvc-striped-s1"].HostID, byName["pvc-striped-s2"].HostID}
	assertDistinctHosts(t, hosts, 3)
	if !strings.Contains(vol.GetVolumeContext()[csicommon.CtxStripeMembers], ","+leftover+",") {
		t.Errorf("members %s do not hold the leftover as member 1", vol.GetVolumeContext()[csicommon.CtxStripeMembers])
	}
}

// TestCreateStripedVolumeRemovesWhatItMadeWhenItCannotFinish is the no-orphans
// rule: a member that cannot be created takes the ones that were with it.
func TestCreateStripedVolumeRemovesWhatItMadeWhenItCannotFinish(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}))

	creates := 0
	mock.failIf = func(r *http.Request) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/volumes/") {
			return false
		}
		creates++
		return creates == 3
	}
	if _, err := cs.CreateVolume(context.Background(), stripedRequest("pvc-striped", "3", 3*gib)); err == nil {
		t.Fatal("CreateVolume succeeded with a member that could not be created")
	}
	if n := len(volumesNamed(mock)); n != 0 {
		t.Errorf("a failed striped create left %d volumes behind: %v", n, volumesNamed(mock))
	}
}

func TestCreateStripedVolumeNeedsANodePerMember(t *testing.T) {
	cases := map[string][]atlascp.StorageNode{
		"fewer nodes than members":   storageNodes([]string{stripeNodeA, stripeNodeB}),
		"an offline node is no node": storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}, stripeNodeC),
	}
	for name, nodes := range cases {
		t.Run(name, func(t *testing.T) {
			mock := newMockSBCLI()
			defer mock.Close()
			cs := stripedServer(t, mock, nodes)
			_, err := cs.CreateVolume(context.Background(), stripedRequest("pvc-striped", "3", 3*gib))
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("CreateVolume = %v, want InvalidArgument", err)
			}
			if n := len(volumesNamed(mock)); n != 0 {
				t.Errorf("a refused create made %d volumes", n)
			}
		})
	}
}

func TestCreateStripedVolumeRefusesWhatItCannotBe(t *testing.T) {
	cases := map[string]map[string]string{
		"client-side compression":   {"client_compression": "true"},
		"client-side deduplication": {"client_deduplication": "true"},
		"replication":               {"replicate": "true"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			mock := newMockSBCLI()
			defer mock.Close()
			cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB}))
			req := stripedRequest("pvc-striped", "2", 2*gib)
			for k, v := range extra {
				req.Parameters[k] = v
			}
			if _, err := cs.CreateVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
				t.Errorf("CreateVolume = %v, want InvalidArgument", err)
			}
			if n := len(volumesNamed(mock)); n != 0 {
				t.Errorf("a refused create made %d volumes", n)
			}
		})
	}
}

func TestDivideQoSSplitsTheClassAcrossTheMembers(t *testing.T) {
	data := &controlplane.CreateLVolData{MaxRWIOPS: "1000", MaxRWmBytes: "", MaxRmBytes: "10", MaxWmBytes: "0"}
	divideQoS(data, 3)
	if data.MaxRWIOPS != "334" || data.MaxRWmBytes != "" || data.MaxRmBytes != "4" || data.MaxWmBytes != "0" {
		t.Errorf("divided ceilings = %+v, want 334, unset, 4, and 0 left alone", data)
	}
}

func TestDeleteStripedVolumeDeletesEveryMember(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}))
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))
	// An unrelated volume survives.
	createSourceVolume(t, cs, "bystander")

	for range 2 { // the second is the retry that finds nothing
		req := &csi.DeleteVolumeRequest{VolumeId: vol.GetVolumeId()}
		if _, err := cs.DeleteVolume(context.Background(), req); err != nil {
			t.Fatalf("DeleteVolume: %v", err)
		}
	}
	byName := volumesNamed(mock)
	if len(byName) != 1 || byName["bystander"] == nil {
		t.Errorf("after delete the control plane holds %v, want the bystander alone", byName)
	}
}

// TestDeleteStripedVolumeFinishesAPartialDelete is the retry of a delete that
// stopped after the last member: the first one is still there to find the rest
// by, and the delete completes.
func TestDeleteStripedVolumeFinishesAPartialDelete(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}))
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))
	mock.mu.Lock()
	for id, v := range mock.volumes {
		if v.Name == "pvc-striped-s2" {
			delete(mock.volumes, id)
		}
	}
	mock.mu.Unlock()

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: vol.GetVolumeId()}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if n := len(volumesNamed(mock)); n != 0 {
		t.Errorf("a resumed delete left %d volumes", n)
	}
}

func TestExpandStripedVolumeGrowsEveryMember(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}))
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))

	resp, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      vol.GetVolumeId(),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 10 * gib},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if resp.GetCapacityBytes() != 12*gib || !resp.GetNodeExpansionRequired() {
		t.Errorf("expand = %+v, want 3 members of 4GiB and node expansion", resp)
	}
	for name, v := range volumesNamed(mock) {
		if v.Size != 4*gib {
			t.Errorf("member %s is %d bytes after the expansion, want 4GiB", name, v.Size)
		}
	}
}

// snapshotStriped takes a snapshot of a striped volume.
func snapshotStriped(t *testing.T, cs *Server, volumeID, name string) *csi.Snapshot {
	t.Helper()
	resp, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{SourceVolumeId: volumeID, Name: name})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	return resp.GetSnapshot()
}

func TestStripedSnapshotCoversEveryMemberAndListsAsOne(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}))
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))
	members := strings.Split(vol.GetVolumeContext()[csicommon.CtxStripeMembers], ",")

	snap := snapshotStriped(t, cs, vol.GetVolumeId(), "snapshot-1")
	if !strings.HasSuffix(snap.GetSnapshotId(), ":s3") || snap.GetSizeBytes() != 3*gib {
		t.Errorf("snapshot = %+v, want a width-3 id and the members' size", snap)
	}

	mock.mu.Lock()
	byName := map[string]*mockSnapshot{}
	for _, s := range mock.snapshots {
		byName[s.Name] = s
	}
	mock.mu.Unlock()
	if len(byName) != 3 {
		t.Fatalf("the control plane holds %d snapshots, want one per member", len(byName))
	}
	for i, member := range members {
		s := byName[stripeMemberName("snapshot-1", i)]
		if s == nil || s.VolUUID != member {
			t.Errorf("member snapshot %d = %+v, want one of member %s", i, s, member)
		}
	}

	// A plain snapshot beside it lists as itself.
	plain := createSourceVolume(t, cs, "plain")
	snapshotStriped(t, cs, plain, "snapshot-plain")

	list, err := cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	ids := make([]string, 0, len(list.GetEntries()))
	for _, e := range list.GetEntries() {
		ids = append(ids, e.GetSnapshot().GetSnapshotId())
		if e.GetSnapshot().GetSnapshotId() == snap.GetSnapshotId() && e.GetSnapshot().GetSizeBytes() != 3*gib {
			t.Errorf("the striped snapshot lists at %d bytes, want 3GiB", e.GetSnapshot().GetSizeBytes())
		}
	}
	if len(ids) != 2 || !slices.Contains(ids, snap.GetSnapshotId()) {
		t.Errorf("ListSnapshots = %v, want the striped snapshot once and the plain one", ids)
	}

	byID, err := cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SnapshotId: snap.GetSnapshotId()})
	if err != nil || len(byID.GetEntries()) != 1 {
		t.Errorf("ListSnapshots by id = %v, %v; want the striped snapshot", byID, err)
	}

	for range 2 { // the second is the retry that finds nothing
		req := &csi.DeleteSnapshotRequest{SnapshotId: snap.GetSnapshotId()}
		if _, err := cs.DeleteSnapshot(context.Background(), req); err != nil {
			t.Fatalf("DeleteSnapshot: %v", err)
		}
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.snapshots) != 1 {
		t.Errorf("after deleting the striped snapshot %d remain, want the plain one", len(mock.snapshots))
	}
}

// TestStripedSnapshotRemovesWhatItTookWhenAMemberFails is the no-orphans rule
// for snapshots: a member that cannot be snapshotted takes the others' with it.
func TestStripedSnapshotRemovesWhatItTookWhenAMemberFails(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}))
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))
	failing := strings.Split(vol.GetVolumeContext()[csicommon.CtxStripeMembers], ",")[1]

	mock.failIf = func(r *http.Request) bool {
		return r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/volumes/"+failing+"/snapshots")
	}
	_, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		SourceVolumeId: vol.GetVolumeId(), Name: "snapshot-1",
	})
	if err == nil {
		t.Fatal("CreateSnapshot succeeded with a member that could not be snapshotted")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.snapshots) != 0 {
		t.Errorf("a failed striped snapshot left %d member snapshots behind", len(mock.snapshots))
	}
}

func TestRestoreAStripedSnapshot(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}))
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))
	snap := snapshotStriped(t, cs, vol.GetVolumeId(), "snapshot-1")
	source := volumesNamed(mock)

	restoreFrom := func(name, width string) *csi.CreateVolumeRequest {
		req := stripedRequest(name, width, 3*gib)
		req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: snap.GetSnapshotId()},
		}}
		return req
	}

	restored := createStriped(t, cs, restoreFrom("pvc-restored", "3"))
	if !strings.HasSuffix(restored.GetVolumeId(), ":s3") {
		t.Errorf("restored volume id %q is not width 3", restored.GetVolumeId())
	}
	byName := volumesNamed(mock)
	for i := range 3 {
		clone := byName[stripeMemberName("pvc-restored", i)]
		from := source[stripeMemberName("pvc-striped", i)]
		if clone == nil || clone.HostID != from.HostID {
			t.Errorf("restored member %d = %+v, want a clone of member %d on its node", i, clone, i)
		}
	}

	_, err := cs.CreateVolume(context.Background(), restoreFrom("pvc-narrow", "2"))
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("restoring a width-3 snapshot at width 2 = %v, want InvalidArgument", err)
	}
	plain := restoreFrom("pvc-plain", "")
	delete(plain.Parameters, "stripe_width")
	if _, err := cs.CreateVolume(context.Background(), plain); status.Code(err) != codes.InvalidArgument {
		t.Errorf("restoring a striped snapshot into an ordinary class = %v, want InvalidArgument", err)
	}
}

func TestCloneAStripedVolume(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB}))
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "2", 2*gib))

	req := stripedRequest("pvc-clone", "2", 2*gib)
	req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
		Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: vol.GetVolumeId()},
	}}
	clone := createStriped(t, cs, req)
	if !strings.HasSuffix(clone.GetVolumeId(), ":s2") {
		t.Errorf("clone id %q is not width 2", clone.GetVolumeId())
	}
	byName := volumesNamed(mock)
	if byName["pvc-clone"] == nil || byName["pvc-clone-s1"] == nil {
		t.Fatalf("volumes = %v, want both clone members", byName)
	}
	if byName["pvc-clone-s1"].HostID != byName["pvc-striped-s1"].HostID {
		t.Errorf("clone member 1 is on %s, want its source's node %s",
			byName["pvc-clone-s1"].HostID, byName["pvc-striped-s1"].HostID)
	}
}

func TestReplicationRefusesAStripedVolume(t *testing.T) {
	cs := &Server{}
	id := sanityClusterID + ":" + sanityPoolUUID + ":a1111111-1111-4111-8111-111111111111:s2"
	_, err := cs.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{
		VolumeId:   id,
		Parameters: map[string]string{replicationPolicyParam: "policy"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("EnableVolumeReplication on a striped volume = %v, want FailedPrecondition", err)
	}
	if _, _, err := parseGroupMembers([]string{id}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a volume group of a striped volume = %v, want FailedPrecondition", err)
	}
}

func TestFoldStripedSnapshots(t *testing.T) {
	snap := func(name string, size int64) *controlplane.SnapshotResp {
		return &controlplane.SnapshotResp{Name: name, UUID: name, Size: size, ClusterID: "c", PoolID: "p"}
	}
	views := foldStripedSnapshots([]*controlplane.SnapshotResp{
		snap("snapshot-a", 1), snap("snapshot-a-s1", 2), snap("snapshot-a-s2", 3),
		snap("snapshot-b", 5),
		// A name that only looks like a member, with no snapshot it belongs to.
		snap("orphan-s1", 7),
	})
	got := map[string]stripedSnapshotView{}
	for _, v := range views {
		got[v.entry.Name] = v
	}
	if len(got) != 3 {
		t.Fatalf("views = %v, want snapshot-a, snapshot-b, and orphan-s1", got)
	}
	if v := got["snapshot-a"]; v.width != 3 || v.size != 6 {
		t.Errorf("snapshot-a = width %d, size %d; want 3 and 6", v.width, v.size)
	}
	if v := got["snapshot-b"]; v.width != 1 || v.size != 5 {
		t.Errorf("snapshot-b = width %d, size %d; want 1 and 5", v.width, v.size)
	}
}
