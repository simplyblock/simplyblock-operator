package lvol

import "testing"

const (
	testClusterID  = "0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90"
	testExportUUID = "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13"
)

func TestParseNFSHandle(t *testing.T) {
	cases := []struct {
		name   string
		handle VolumeHandle
		want   NFSHandle
		ok     bool
	}{
		{
			name:   "the pNFS form",
			handle: VolumeHandle("nfs:" + testClusterID + ":pool-a:" + testExportUUID),
			want:   NFSHandle{ClusterID: testClusterID, PoolRef: "pool-a", ExportUUID: testExportUUID},
			ok:     true,
		},
		{
			// The pool segment is a name on volumes provisioned before the v2
			// API migration, exactly as it is for the three-part form.
			name:   "a pool UUID is equally acceptable",
			handle: VolumeHandle("nfs:" + testClusterID + ":" + testClusterID + ":" + testExportUUID),
			want: NFSHandle{
				ClusterID: testClusterID, PoolRef: testClusterID, ExportUUID: testExportUUID,
			},
			ok: true,
		},
		{
			// A handle is read back out of a PersistentVolume, which is a YAML
			// document a human may have edited.
			name:   "surrounding whitespace is transport, not content",
			handle: VolumeHandle("  nfs:" + testClusterID + ":pool-a:" + testExportUUID + "\n"),
			want:   NFSHandle{ClusterID: testClusterID, PoolRef: "pool-a", ExportUUID: testExportUUID},
			ok:     true,
		},
		{
			name:   "a three-part lvol handle is not a pNFS handle",
			handle: VolumeHandle(testClusterID + ":pool-a:" + testExportUUID),
		},
		{
			name:   "another prefix is another driver's",
			handle: VolumeHandle("iscsi:" + testClusterID + ":pool-a:" + testExportUUID),
		},
		{
			name:   "the prefix is matched exactly, not case-folded",
			handle: VolumeHandle("NFS:" + testClusterID + ":pool-a:" + testExportUUID),
		},
		{
			name:   "a non-canonical cluster id is rejected",
			handle: VolumeHandle("nfs:not-a-uuid:pool-a:" + testExportUUID),
		},
		{
			name:   "a non-canonical export uuid is rejected",
			handle: VolumeHandle("nfs:" + testClusterID + ":pool-a:not-a-uuid"),
		},
		{
			name:   "an empty pool segment is rejected",
			handle: VolumeHandle("nfs:" + testClusterID + "::" + testExportUUID),
		},
		{
			name:   "a fifth segment is rejected",
			handle: VolumeHandle("nfs:" + testClusterID + ":pool-a:" + testExportUUID + ":extra"),
		},
		{
			name:   "the empty handle is rejected",
			handle: VolumeHandle(""),
		},
		{
			name:   "the prefix alone is rejected",
			handle: VolumeHandle("nfs:"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseNFSHandle(tc.handle)
			if ok != tc.ok {
				t.Fatalf("ParseNFSHandle(%q) ok = %v, want %v", tc.handle, ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("ParseNFSHandle(%q) = %+v, want %+v", tc.handle, got, tc.want)
			}
		})
	}
}

// The three-part form is what every existing caller of ParseHandle reads, and
// each one treats Handle.VolumeID as an lvol UUID. A pNFS handle names an
// export rather than an lvol, so ParseHandle rejecting it is what keeps an
// export UUID from reaching code that will use it to address an lvol.
func TestParseHandleRejectsTheNFSForm(t *testing.T) {
	h := NewNFSVolumeHandle(testClusterID, "pool-a", testExportUUID)
	if got, ok := ParseHandle(h); ok {
		t.Errorf("ParseHandle(%q) = %+v, ok; want it rejected as not an lvol handle", h, got)
	}
}

func TestNFSHandleRoundTrip(t *testing.T) {
	want := NFSHandle{ClusterID: testClusterID, PoolRef: "pool-a", ExportUUID: testExportUUID}

	got, ok := ParseNFSHandle(want.Handle())
	if !ok {
		t.Fatalf("ParseNFSHandle(%q) rejected a handle this package rendered", want.Handle())
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// NewNFSVolumeHandle is the one place that knows the encoding, so what it
// writes and what String renders have to be the same bytes.
func TestNewNFSVolumeHandleMatchesString(t *testing.T) {
	h := NFSHandle{ClusterID: testClusterID, PoolRef: "pool-a", ExportUUID: testExportUUID}
	want := "nfs:" + testClusterID + ":pool-a:" + testExportUUID

	if got := h.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := NewNFSVolumeHandle(testClusterID, "pool-a", testExportUUID); string(got) != want {
		t.Errorf("NewNFSVolumeHandle() = %q, want %q", got, want)
	}
}

// IsNFS is the cheap discriminator a caller holding an arbitrary handle uses to
// pick a parser. It answers on the shape alone, so a malformed pNFS handle is
// still recognizably one, and is rejected by ParseNFSHandle rather than
// mistaken for an lvol handle.
func TestVolumeHandleIsNFS(t *testing.T) {
	cases := []struct {
		handle VolumeHandle
		want   bool
	}{
		{NewNFSVolumeHandle(testClusterID, "pool-a", testExportUUID), true},
		{VolumeHandle("nfs:garbage"), true},
		{VolumeHandle("  nfs:" + testClusterID + ":pool-a:" + testExportUUID), true},
		{VolumeHandle(testClusterID + ":pool-a:" + testExportUUID), false},
		{VolumeHandle("nfsx:" + testClusterID + ":pool-a:" + testExportUUID), false},
		{VolumeHandle("NFS:" + testClusterID + ":pool-a:" + testExportUUID), false},
		{VolumeHandle(""), false},
	}
	for _, tc := range cases {
		if got := tc.handle.IsNFS(); got != tc.want {
			t.Errorf("VolumeHandle(%q).IsNFS() = %v, want %v", tc.handle, got, tc.want)
		}
	}
}
