// What a volume-create request puts on the wire, for the fields where the
// difference between "absent" and "empty" is the difference between a volume
// and a 422.
package controlplane

import (
	"encoding/json"
	"testing"
)

// TestNoEmptyStringIsSent is the guard for a whole class of bug rather than one
// field of it.
//
// Not one optional string in this request has an empty value that means
// anything: an unstated ceiling is no ceiling, an unstated fabric is the
// cluster's, an unstated host is any host. The control plane agrees — each is
// declared with a default or read behind a falsy check — but a default only
// applies to an absent key, and a key present with "" is a value somebody
// chose. The four ceilings are validated as integers, so "" is a 422 and the
// claim never binds. The fabric is worse: "" parses, and reaches the attribute
// name active_<fabric>, which names nothing.
//
// Asserting the shape rather than a list of fields is the point. A field added
// later with the same spelling is the same bug, and it would pass a test that
// only knew today's names.
func TestNoEmptyStringIsSent(t *testing.T) {
	// Only what a caller must always supply. Everything else is an absence, and
	// an absence is what this asserts about.
	body, err := json.Marshal(&CreateLVolData{
		LvolName: "pvc-0d3a",
		Size:     "1073741824",
		LvsName:  "pool1",
	})
	if err != nil {
		t.Fatalf("marshalling the create request: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("reading the create request back: %v", err)
	}

	for key, value := range sent {
		if s, ok := value.(string); ok && s == "" {
			t.Errorf("%q was sent as an empty string; the control plane reads that as a value "+
				"stated rather than a value omitted, and its own default never applies", key)
		}
	}
}

// TestAStatedValueIsSent is the other half: omitempty must not swallow what a
// StorageClass actually asked for.
func TestAStatedValueIsSent(t *testing.T) {
	body, err := json.Marshal(&CreateLVolData{
		LvolName:    "pvc-0d3a",
		Size:        "1073741824",
		LvsName:     "pool1",
		Fabric:      "rdma",
		MaxRWIOPS:   "1000",
		MaxRWmBytes: "100",
		HostID:      "node-7",
		LvolID:      "3f9c",
		PvcName:     "ns/claim",
	})
	if err != nil {
		t.Fatalf("marshalling the create request: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("reading the create request back: %v", err)
	}

	for key, want := range map[string]string{
		"fabric":        "rdma",
		"max_rw_iops":   "1000",
		"max_rw_mbytes": "100",
		"host_id":       "node-7",
		"uid":           "3f9c",
		"pvc_name":      "ns/claim",
		"name":          "pvc-0d3a",
		"size":          "1073741824",
		"pool":          "pool1",
	} {
		if sent[key] != want {
			t.Errorf("%s was sent as %v, want %q", key, sent[key], want)
		}
	}
}

// TestAZeroIsStillSent pins the fields omitempty must not be extended to. A
// ceiling of 0 is unlimited and false is a choice, so their keys have to travel
// even at their zero values.
func TestAZeroIsStillSent(t *testing.T) {
	body, err := json.Marshal(&CreateLVolData{LvolName: "pvc-0d3a", Size: "1", LvsName: "pool1"})
	if err != nil {
		t.Fatalf("marshalling the create request: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("reading the create request back: %v", err)
	}

	for _, key := range []string{"encrypt", "namespaced", "max_namespace_per_subsys"} {
		if _, ok := sent[key]; !ok {
			t.Errorf("%q was omitted; its zero value is a value, not an absence", key)
		}
	}
}
