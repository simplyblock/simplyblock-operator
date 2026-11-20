package v1alpha2

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression: 2026-09-24-nfsexport-encrypted-omitempty -- Encrypted carried
// `,omitempty` and is `+k8s:immutable`. The CRD enforces immutability by
// requiring the field stay present once set, and the CSI controller's
// unstructured Create always writes it explicitly, so an unencrypted export's
// record has `encrypted: false` in etcd from the start. omitempty then drops
// that same false value from every later write a typed client makes -- the
// reconciler's own finalizer add among them -- which the CRD reads as the
// field being removed, refusing the write. On a real API server (never on the
// fake client these packages test against elsewhere) this took every
// unencrypted export's reconcile down permanently, first observed against a
// live cluster.
func TestNFSExportSpecEncryptedSurvivesAFalseRoundTrip(t *testing.T) {
	spec := NFSExportSpec{
		VolumeRef:  "0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90:pool-a:3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13",
		ExportPath: "/var/lib/simplyblock/exports/x",
		Encrypted:  false,
	}
	out, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), `"encrypted":false`) {
		t.Fatalf("encrypted:false was dropped from the encoding: %s", out)
	}
}
