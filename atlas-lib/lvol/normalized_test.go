package lvol

import (
	"strings"
	"testing"
)

// The rule of §16.4: an annotation is taken when it agrees with the field about
// everything but the pool, and ignored otherwise.
//
// The cases that matter are the disagreements. A handle names a cluster and a
// volume as well as a pool, and an annotation is metadata anybody with edit
// rights can write, so an annotation that redirects either of those is the one
// thing this must not honor.
func TestNormalizeHandle(t *testing.T) {
	const (
		cluster      = "8ffac363-0c46-4714-a71b-f9c0b58a1269"
		otherCluster = "11111111-2222-4333-8444-555555555555"
		poolUUID     = "df34f16c-1a2b-3c4d-5e6f-7a8b9c0d1e2f"
		volume       = "a1111111-1111-4111-8111-111111111111"
		otherVolume  = "b2222222-2222-4222-8222-222222222222"
	)

	legacy := VolumeHandle(cluster + ":production:" + volume)
	normalized := VolumeHandle(cluster + ":" + poolUUID + ":" + volume)

	for _, tc := range []struct {
		name      string
		field     VolumeHandle
		annotated VolumeHandle
		wantPool  string
		wantFrom  bool
		wantWhy   string
	}{
		{
			name:     "no annotation leaves the field as it is",
			field:    legacy,
			wantPool: "production",
		},
		{
			name:      "an annotation differing only in the pool is taken",
			field:     legacy,
			annotated: normalized,
			wantPool:  poolUUID,
			wantFrom:  true,
		},
		{
			name:      "an annotation equal to the field is taken and changes nothing",
			field:     normalized,
			annotated: normalized,
			wantPool:  poolUUID,
			wantFrom:  true,
		},
		{
			name:      "an annotation naming another cluster is ignored",
			field:     legacy,
			annotated: VolumeHandle(otherCluster + ":" + poolUUID + ":" + volume),
			wantPool:  "production",
			wantWhy:   "cluster",
		},
		{
			name:      "an annotation naming another volume is ignored",
			field:     legacy,
			annotated: VolumeHandle(cluster + ":" + poolUUID + ":" + otherVolume),
			wantPool:  "production",
			wantWhy:   "volume",
		},
		{
			name:      "an annotation that is not a handle is ignored",
			field:     legacy,
			annotated: "not-a-handle",
			wantPool:  "production",
			wantWhy:   "well formed",
		},
		{
			name:      "an empty annotation is absent rather than wrong",
			field:     legacy,
			annotated: "   ",
			wantPool:  "production",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeHandle(tc.field, tc.annotated)
			if !ok {
				t.Fatalf("NormalizeHandle(%q, %q) = not ok, want ok", tc.field, tc.annotated)
			}
			if got.Handle.PoolRef != tc.wantPool {
				t.Errorf("pool = %q, want %q", got.Handle.PoolRef, tc.wantPool)
			}
			if got.FromAnnotation != tc.wantFrom {
				t.Errorf("FromAnnotation = %v, want %v", got.FromAnnotation, tc.wantFrom)
			}
			if tc.wantWhy == "" && got.Ignored != "" {
				t.Errorf("Ignored = %q, want none", got.Ignored)
			}
			if tc.wantWhy != "" && !strings.Contains(got.Ignored, tc.wantWhy) {
				t.Errorf("Ignored = %q, want it to mention %q", got.Ignored, tc.wantWhy)
			}
		})
	}
}

// A field that is not a handle leaves nothing to normalize, whatever the
// annotation says. The annotation is a record about the field, so it cannot
// stand in for one that is missing.
func TestNormalizeHandleRefusesAnUnreadableField(t *testing.T) {
	const good = "8ffac363-0c46-4714-a71b-f9c0b58a1269:" +
		"df34f16c-1a2b-3c4d-5e6f-7a8b9c0d1e2f:a1111111-1111-4111-8111-111111111111"

	if _, ok := NormalizeHandle("", good); ok {
		t.Error("an empty field was normalized from an annotation, which lets metadata " +
			"invent a volume the spec does not name")
	}
	if _, ok := NormalizeHandle("nonsense", good); ok {
		t.Error("an unparsable field was normalized from an annotation")
	}
}
