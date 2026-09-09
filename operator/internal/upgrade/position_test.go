// Tests for the positioning. §27 has the read-only command report the plan for
// whichever phase the cluster is positioned for, and it reads that rather than
// being told, so what is asserted is that the reading matches §7.4's staging.

package upgrade

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// crdFor builds the CustomResourceDefinition of one converting kind, serving
// the versions named.
func crdFor(kind ConvertingKind, versions ...string) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: kind.CRDName()},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: APIGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind.Kind, Plural: kind.Plural},
		},
	}
	for _, version := range versions {
		crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{
			Name: version, Served: true,
		})
	}
	return crd
}

// positionOver reads the position of a cluster holding these CRDs.
func positionOver(t *testing.T, objects ...client.Object) Position {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering apiextensions: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	scope := NewScope(c, "simplyblock", StagePreflight, Options{}, logf.Log, DiscardReporter{})

	position, err := Positioned(t.Context(), scope)
	if err != nil {
		t.Fatalf("Positioned: %v", err)
	}
	return position
}

// converting builds one CRD per converting kind, serving these versions.
func converting(versions ...string) []client.Object {
	kinds := ConvertingKinds()
	out := make([]client.Object, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, crdFor(kind, versions...))
	}
	return out
}

func TestPositioned_ACLusterServingOnlyTheOldVersionIsForUpgrade(t *testing.T) {
	position := positionOver(t, converting(VersionOld)...)

	if position.Stage != StageUpgrade {
		t.Fatalf("stage = %q, want %q", position.Stage, StageUpgrade)
	}
	if !strings.Contains(position.Because, "cannot run an operator") {
		t.Errorf("the reason does not say why: %q", position.Because)
	}
	if len(position.Pending) != len(ConvertingKinds()) {
		t.Errorf("pending = %v, want every converting kind", position.Pending)
	}
}

func TestPositioned_ACLusterServingBothIsForMigrate(t *testing.T) {
	// §7.4's staging: the upgrade leaves both versions served, and the resource
	// model is what is left to change.
	position := positionOver(t, converting(VersionOld, VersionNew)...)

	if position.Stage != StageMigrate {
		t.Fatalf("stage = %q, want %q", position.Stage, StageMigrate)
	}
	if len(position.Pending) != 0 {
		t.Errorf("pending = %v, want none", position.Pending)
	}
}

func TestPositioned_APartiallyAppliedSetIsForUpgradeAndSaysSo(t *testing.T) {
	// §11 refuses to proceed past this: it leaves the operator reconciling one
	// kind at each version. The upgrade is the command that would finish it.
	kinds := ConvertingKinds()
	objects := make([]client.Object, 0, len(kinds))
	objects = append(objects, crdFor(kinds[0], VersionOld, VersionNew))
	for _, kind := range kinds[1:] {
		objects = append(objects, crdFor(kind, VersionOld))
	}

	position := positionOver(t, objects...)
	if position.Stage != StageUpgrade {
		t.Fatalf("stage = %q, want %q", position.Stage, StageUpgrade)
	}
	if !position.Partial() {
		t.Fatal("a set applied to one kind and not the rest was not reported as partial")
	}
	if !strings.Contains(position.Because, "one kind at each version") {
		t.Errorf("the reason does not say what is wrong with it: %q", position.Because)
	}
}

func TestPositioned_AnAbsentCRDCountsAsNotYetUpgraded(t *testing.T) {
	// The group not being installed is a prerequisite failure with its own
	// report, and answering "not upgraded yet" here is the safe direction to be
	// wrong in.
	position := positionOver(t)

	if position.Stage != StageUpgrade {
		t.Fatalf("stage = %q, want %q on a cluster with no CRDs at all", position.Stage, StageUpgrade)
	}
}

func TestPositioned_AVersionPresentButNotServedDoesNotCount(t *testing.T) {
	// §28 stops serving the old version before the CRD drops it, so presence
	// and service are different questions and only one of them means the
	// upgrade happened.
	kinds := ConvertingKinds()
	objects := make([]client.Object, 0, len(kinds))
	for _, kind := range kinds {
		crd := crdFor(kind, VersionOld)
		crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{
			Name: VersionNew, Served: false,
		})
		objects = append(objects, crd)
	}

	if position := positionOver(t, objects...); position.Stage != StageUpgrade {
		t.Fatalf("stage = %q, want %q: a version that is present and not served "+
			"is one no client can read", position.Stage, StageUpgrade)
	}
}

func TestConvertingKinds_AreTheSevenOfTheInventory(t *testing.T) {
	kinds := ConvertingKinds()
	if len(kinds) != 7 {
		t.Fatalf("got %d converting kinds, and §7.2 names seven", len(kinds))
	}

	seen := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		if seen[kind.Kind] {
			t.Errorf("%s is in the inventory twice", kind.Kind)
		}
		seen[kind.Kind] = true

		if kind.Plural == "" {
			t.Errorf("%s names no plural, and its CRD cannot be looked up", kind.Kind)
		}
		if !strings.HasSuffix(kind.CRDName(), "."+APIGroup) {
			t.Errorf("%s resolves to CRD %q, which is not in this group", kind.Kind, kind.CRDName())
		}
	}
}
