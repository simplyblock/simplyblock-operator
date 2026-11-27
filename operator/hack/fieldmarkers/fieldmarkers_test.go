// Tests of the field-marker generator against the fixture API packages under
// testdata/: the paths each marker resolves to, and the annotations written
// from them.

package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestCollectResolvesEveryMarkedPath(t *testing.T) {
	kinds, err := collect("./testdata/fixture/...")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	want := Kinds{
		"fixture.simplyblock.io/Widget": {
			markerOperatorManaged: {"v1beta1": {
				"spec.byName.serial", "spec.inherited", "spec.owner", "spec.parts.serial", "spec.tree.label", "status.phase",
			}},
			markerInvisible: {"v1beta1": {
				"spec.bookkeeping", "spec.byName.serial", "spec.detail.token", "spec.parts.serial", "spec.tuning.token",
			}},
			markerAdvanced: {"v1beta1": {"spec.tuning"}},
		},
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("collect resolved\n  %v\nwant\n  %v", kinds, want)
	}
}

func TestCollectRefusesAnUnknownMarker(t *testing.T) {
	_, err := collect("./testdata/typo/...")
	if err == nil || !strings.Contains(err.Error(), "+simplyblock:operatorManged") {
		t.Fatalf("collect error = %v, want one naming the unknown marker", err)
	}
}

// make manifests runs before make generate, so an API package whose deepcopy
// still names a removed field has to resolve all the same.
func TestCollectToleratesTypeErrors(t *testing.T) {
	kinds, err := collect("./testdata/stale/...")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	want := Kinds{"stale.simplyblock.io/Gizmo": {markerOperatorManaged: {"v1": {"spec.owner"}}}}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("collect resolved %v, want %v", kinds, want)
	}
}

func TestAnnotationsArePerVersionJSON(t *testing.T) {
	got := annotations(Fields{
		markerOperatorManaged: {"v1alpha2": {"spec.a", "spec.b"}, "v1alpha1": {"spec.a"}},
		markerAdvanced:        {"v1alpha2": {"spec.c"}},
	})
	want := map[string]string{
		"simplyblock.io/operator-managed-fields": `{"v1alpha1":["spec.a"],"v1alpha2":["spec.a","spec.b"]}`,
		"simplyblock.io/advanced-fields":         `{"v1alpha2":["spec.c"]}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("annotations = %v, want %v", got, want)
	}
}
