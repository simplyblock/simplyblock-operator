// The rule this package states, held against the operator's own source, and the
// scan itself held against a file that plainly breaks it.
//
// The second test is what keeps the first honest: a scan that silently found
// nothing would pass the operator's tree just as convincingly as a scan that
// works.

package storedversion

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	apiruntime "k8s.io/apimachinery/pkg/runtime"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// convertedKinds are the kinds both versions define, which is exactly the set
// that has moved: v1alpha2 is the stored version of every kind it carries, so a
// kind present in both is one whose v1alpha1 is served by conversion alone.
//
// It is derived from the schemes rather than listed, so a kind that moves after
// this is written is guarded the day it moves.
func convertedKinds(t *testing.T) map[string]bool {
	t.Helper()

	kindsOf := func(add func(*apiruntime.Scheme) error) map[string]bool {
		s := apiruntime.NewScheme()
		if err := add(s); err != nil {
			t.Fatalf("building a scheme: %v", err)
		}
		// Only the kinds the API package itself defines. AddToScheme also
		// registers the metav1 option and list types into the group, and those
		// are nobody's stored version.
		kinds := map[string]bool{}
		for gvk, goType := range s.AllKnownTypes() {
			if gvk.Group != "storage.simplyblock.io" {
				continue
			}
			if !strings.HasPrefix(goType.PkgPath(), "github.com/simplyblock/simplyblock-operator/api/") {
				continue
			}
			kinds[gvk.Kind] = true
		}
		return kinds
	}

	old := kindsOf(simplyblockv1alpha1.AddToScheme)
	converted := map[string]bool{}
	for kind := range kindsOf(simplyblockv1alpha2.AddToScheme) {
		if old[kind] {
			converted[kind] = true
		}
	}
	if len(converted) == 0 {
		t.Fatal("no kind is defined in both versions, so the scan would assert nothing")
	}
	return converted
}

// Regression: 2026-09-11-operator-reads-retired-crd-versions. Three reads stayed
// on v1alpha1 after v1alpha2 became the stored version: a Get, two Watch
// registrations, and an admission guard. The Get was caught by a reconcile test
// and the registrations by nothing, since a controller's wiring is not exercised
// by calling its reconcile function. This asserts the rule they all broke, which
// covers a registration and a fixture as readily as a call.
func TestTheOperatorReadsEveryKindAtItsStoredVersion(t *testing.T) {
	kinds := convertedKinds(t)
	t.Logf("kinds guarded: %v", sortedKeys(kinds))

	findings, err := Scan(operatorRoot(t), kinds)
	if err != nil {
		t.Fatalf("scanning the operator: %v", err)
	}
	for _, f := range findings {
		t.Errorf("%s", f)
	}
}

// The scan proven against a file that breaks the rule, so that a pass above
// means the rule holds rather than that nothing was read.
func TestTheScanFindsARetiredVersionRead(t *testing.T) {
	dir := t.TempDir()
	source := `package example

import (
	v1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func read() *v1.ControlPlane { return &v1.ControlPlane{} }
`
	if err := os.WriteFile(filepath.Join(dir, "example.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("writing the example: %v", err)
	}

	findings, err := Scan(dir, map[string]bool{"ControlPlane": true})
	if err != nil {
		t.Fatalf("scanning the example: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("expected both references to be reported, got %d: %v", len(findings), findings)
	}
	if findings[0].Kind != "ControlPlane" || findings[0].File != "example.go" {
		t.Fatalf("unexpected finding: %+v", findings[0])
	}
}

// A kind that has not moved is not reported, which is what keeps the rule from
// reading as "v1alpha1 is forbidden": StorageCluster and StorageNodeSet are
// v1alpha1 and are stored there.
func TestTheScanIgnoresAKindThatHasNotMoved(t *testing.T) {
	dir := t.TempDir()
	source := `package example

import (
	v1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func read() *v1.StorageCluster { return &v1.StorageCluster{} }
`
	if err := os.WriteFile(filepath.Join(dir, "example.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("writing the example: %v", err)
	}

	findings, err := Scan(dir, map[string]bool{"ControlPlane": true})
	if err != nil {
		t.Fatalf("scanning the example: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected no finding, got %v", findings)
	}
}

// operatorRoot is the module root, found from this file rather than from the
// working directory, which `go test` sets to the package under test.
func operatorRoot(t *testing.T) string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
