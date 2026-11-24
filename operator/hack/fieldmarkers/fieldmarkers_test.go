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
	kinds, err := collect(".", "./testdata/fixture/...")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	want := Kinds{
		"fixture.simplyblock.io/Widget": {
			markerOperatorManaged: {"v1beta1": {
				"spec.byName.serial", "spec.inherited", "spec.owner", "spec.parts.serial", "spec.tree.label", "status.phase",
			}},
			markerInvisible: {"v1beta1": {
				"spec.bookkeeping", "spec.byName.serial", "spec.detail.token", "spec.parts.serial",
			}},
		},
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("collect resolved\n  %v\nwant\n  %v", kinds, want)
	}
}

func TestCollectRefusesAnUnknownMarker(t *testing.T) {
	_, err := collect(".", "./testdata/typo/...")
	if err == nil || !strings.Contains(err.Error(), "+simplyblock:operatorManged") {
		t.Fatalf("collect error = %v, want one naming the unknown marker", err)
	}
}

func TestAnnotationsArePerVersionJSON(t *testing.T) {
	got := annotations(Fields{
		markerOperatorManaged: {"v1alpha2": {"spec.a", "spec.b"}, "v1alpha1": {"spec.a"}},
	})
	want := map[string]string{
		"simplyblock.io/operator-managed-fields": `{"v1alpha1":["spec.a"],"v1alpha2":["spec.a","spec.b"]}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("annotations = %v, want %v", got, want)
	}
}
