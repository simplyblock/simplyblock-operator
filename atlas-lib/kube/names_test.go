// Tests for the naming rules the operator and the CSI driver have to spell
// alike, checked against the validators the API server itself uses.
package kube

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// TestPoolNodeLabelKey test to make sure that `PoolNodeLabelKey` doesn't generate label key more 63 bits
func TestPoolNodeLabelKey(t *testing.T) {
	const uuid = "432ef113-aa65-46a0-9210-67d92cb955e4"

	key := PoolNodeLabelKey(uuid)
	if errs := validation.IsQualifiedName(key); len(errs) != 0 {
		t.Fatalf("PoolNodeLabelKey(%q) = %q, not a valid label name: %s",
			uuid, key, strings.Join(errs, "; "))
	}
	if !strings.HasPrefix(key, LabelPoolPrefix) {
		t.Errorf("PoolNodeLabelKey(%q) = %q, want the %q prefix the CSI node plugin scans for",
			uuid, key, LabelPoolPrefix)
	}
	if key == PoolNodeLabelKey("9c1e4d7a-3f52-4b18-8a6d-2e7f0b5c4a91") {
		t.Errorf("two pools share the key %q, so a node in both cannot be told apart", key)
	}
	if errs := validation.IsValidLabelValue(LabelPoolAllowed); len(errs) != 0 {
		t.Errorf("LabelPoolAllowed = %q is not a valid label value: %s",
			LabelPoolAllowed, strings.Join(errs, "; "))
	}
}
