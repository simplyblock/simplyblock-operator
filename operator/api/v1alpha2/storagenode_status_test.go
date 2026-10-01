// What a StorageNode's status says when the control plane reports it unhealthy.
//
// kubectl prints a column by reading a path out of the stored object, so a value
// that is never written has nothing to print. These pin that false is written.

package v1alpha2

import (
	"encoding/json"
	"testing"
)

// Regression: 2026-10-01-storagenode-health-blank — Health was omitted when
// false, so a node the control plane had not yet reported healthy, or reported
// unhealthy, showed an empty HEALTH column, indistinguishable from a node that
// had never been read. The reading is a statement either way and has to be
// stored as one.
func TestAnUnhealthyNodeStatusStoresFalseRatherThanNothing(t *testing.T) {
	raw, err := json.Marshal(StorageNodeStatus{Status: "online", Health: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, present := stored["health"]
	if !present {
		t.Fatalf("status has no health key, so the HEALTH column is blank: %s", raw)
	}
	if got != false {
		t.Errorf("health = %v, want false", got)
	}
}
