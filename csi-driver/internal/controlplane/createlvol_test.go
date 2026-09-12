// What a volume-create request puts on the wire, for the fields where the
// difference between "absent" and "empty" is the difference between a working
// volume and a broken one.
package controlplane

import (
	"encoding/json"
	"testing"
)

// TestAnUnsetFabricIsNotSent pins the one field whose empty value must not be
// transmitted.
//
// The control plane defaults fabric to tcp and applies that default only when
// the key is missing. A class that names no fabric would otherwise send
// "fabric": "", which reaches add_lvol_ha and is used to build the attribute
// name active_<fabric> that the node's liveness is read from — active_ names
// nothing, and the volume is never created. Class parameters are immutable, so
// a class authored without a fabric can never be repaired.
func TestAnUnsetFabricIsNotSent(t *testing.T) {
	body, err := json.Marshal(&CreateLVolData{LvolName: "vol", Size: "1073741824", LvsName: "pool1"})
	if err != nil {
		t.Fatalf("marshalling the create request: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("reading the create request back: %v", err)
	}
	if _, ok := sent["fabric"]; ok {
		t.Errorf("an unset fabric was sent as %q, which the control plane reads as a fabric "+
			"named rather than a fabric unstated", sent["fabric"])
	}
}

// TestAStatedFabricIsSent is the other half: omitempty must not swallow a class
// that does name a fabric.
func TestAStatedFabricIsSent(t *testing.T) {
	body, err := json.Marshal(&CreateLVolData{LvolName: "vol", LvsName: "pool1", Fabric: "rdma"})
	if err != nil {
		t.Fatalf("marshalling the create request: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("reading the create request back: %v", err)
	}
	if sent["fabric"] != "rdma" {
		t.Errorf("fabric was sent as %v, want rdma", sent["fabric"])
	}
}
