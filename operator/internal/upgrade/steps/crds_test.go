// Tests for §11's CRD installation.
//
// What they hold still is the decision not to write: a CRD is what every custom
// resource is served through, so the step that rewrites one which already holds
// what it should has turned a no-operation into the riskiest write in the
// upgrade.

package steps

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/crds"
)

// The two steps under test. They are values rather than composite literals at
// every call site because a composite literal cannot open an `if` condition.
var (
	applier  applyCRDs
	verifier verifyCRDVersions
)

// oneCRD is an embedded CRD to test against, picked from the set rather than
// named, so that renaming a kind does not break these tests.
func oneCRD(t *testing.T) crds.Definition {
	t.Helper()

	loaded, err := crds.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("no CRDs are embedded")
	}
	return loaded[0]
}

// installed renders an embedded CRD as one the cluster already holds, carrying
// the conditions the API server sets once it has accepted it and the
// spec.conversion it defaults.
func installed(definition crds.Definition) *apiextensionsv1.CustomResourceDefinition {
	crd := definition.Object.DeepCopy()
	crd.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter}

	storage, _ := definition.Storage()
	crd.Status = apiextensionsv1.CustomResourceDefinitionStatus{
		Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{
			{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue},
			{Type: apiextensionsv1.NamesAccepted, Status: apiextensionsv1.ConditionTrue},
		},
		StoredVersions: []string{storage},
	}
	return crd
}

func TestApplyCRDs_SubjectsAreTheEmbeddedSet(t *testing.T) {
	loaded, err := crds.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	subjects, err := applier.Subjects(t.Context(), migration(t))
	if err != nil {
		t.Fatalf("Subjects: %v", err)
	}
	if len(subjects) != len(loaded) {
		t.Fatalf("%d subjects for %d embedded CRDs", len(subjects), len(loaded))
	}

	for i, subject := range subjects {
		if subject.Ref.Name != loaded[i].Name() {
			t.Errorf("subject %d is %s, want %s", i, subject.Ref.Name, loaded[i].Name())
		}
		if subject.Ref.Namespace != "" {
			t.Errorf("%s carries a namespace, and a CRD is cluster-scoped", subject.Ref.Name)
		}
		if subject.IsUpgrade() {
			t.Errorf("%s reads as the upgrade rather than as an object", subject.Ref.Name)
		}
	}
}

func TestApplyCRDs_CreatesOneTheClusterDoesNotHave(t *testing.T) {
	definition := oneCRD(t)
	scope := migration(t)

	action, err := applier.Describe(t.Context(), scope, subjectFor(definition))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if action == nil {
		t.Fatal("described nothing for a CRD that is not installed")
	}
	if action.Verb != upgrade.VerbCreate {
		t.Errorf("verb = %s, want CREATE", action.Verb)
	}
	if !strings.Contains(action.Detail, "stored") {
		t.Errorf("detail = %q, want it to name the stored version", action.Detail)
	}
}

func TestApplyCRDs_LeavesOneTheClusterAlreadyHolds(t *testing.T) {
	// §11: applying a CRD whose content already matches is a write that can
	// only introduce risk.
	definition := oneCRD(t)
	scope := migration(t, installed(definition))
	subject := subjectFor(definition)

	action, err := applier.Describe(t.Context(), scope, subject)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if action != nil {
		t.Errorf("described %s for a CRD that is already the one this binary carries", action.Verb)
	}

	done, err := applier.Done(t.Context(), scope, subject)
	if err != nil {
		t.Fatalf("Done: %v", err)
	}
	if !done {
		t.Error("it is not finished, so the step would report having nothing to do rather than being done")
	}
}

func TestApplyCRDs_UpdatesOneInstalledWithAnotherSchema(t *testing.T) {
	definition := oneCRD(t)
	live := installed(definition)
	live.Spec.Versions[0].Deprecated = true

	action, err := applier.Describe(t.Context(), migration(t, live), subjectFor(definition))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if action == nil {
		t.Fatal("described nothing for a CRD whose schema differs")
	}
	if action.Verb != upgrade.VerbUpdate {
		t.Errorf("verb = %s, want UPDATE", action.Verb)
	}
	// The versions did not move, so saying them with an arrow between two
	// identical halves would report a change that is not the one being made.
	if strings.Contains(action.Detail, "→") {
		t.Errorf("detail = %q, and the versions are unchanged", action.Detail)
	}
}

func TestApplyCRDs_NamesTheVersionsWhenTheyMove(t *testing.T) {
	definition := oneCRD(t)
	live := installed(definition)
	live.Spec.Versions[0].Name = "v1alpha0"

	action, err := applier.Describe(t.Context(), migration(t, live), subjectFor(definition))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if action == nil {
		t.Fatal("described nothing for a CRD serving another version")
	}
	if !strings.Contains(action.Detail, "v1alpha0 (stored) → ") {
		t.Errorf("detail = %q, want the versions it moves between", action.Detail)
	}
}

func TestApplyCRDs_WhatItAppliesIsWhatItThenLeavesAlone(t *testing.T) {
	// The round trip is the point. A CRD written and then read back as
	// something the step does not recognize would make every run rewrite every
	// CRD, which is the write §11 exists to avoid.
	definition := oneCRD(t)
	scope := migration(t)
	subject := subjectFor(definition)

	if err := applier.Apply(t.Context(), scope, subject); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var live apiextensionsv1.CustomResourceDefinition
	if err := scope.Client.Get(t.Context(), types.NamespacedName{Name: definition.Name()}, &live); err != nil {
		t.Fatalf("reading it back: %v", err)
	}

	action, err := applier.Describe(t.Context(), scope, subject)
	if err != nil {
		t.Fatalf("Describe after Apply: %v", err)
	}
	if action != nil {
		t.Errorf("it still describes %s after being applied", action.Verb)
	}
}

func TestApplyCRDs_ValidateRefusesDroppingAStoredVersion(t *testing.T) {
	// A CRD that no longer declares a version objects are stored in is one the
	// API server accepts and then cannot read the kind through.
	definition := oneCRD(t)
	live := installed(definition)
	live.Status.StoredVersions = append(live.Status.StoredVersions, "v1alpha0")

	err := applier.Validate(t.Context(), migration(t, live), subjectFor(definition))
	if err == nil {
		t.Fatal("Validate accepted a CRD that drops a stored version")
	}
	if !strings.Contains(err.Error(), "v1alpha0") {
		t.Errorf("error is %q, want it to name the version", err)
	}
}

func TestApplyCRDs_ValidateAcceptsTheVersionsItDeclares(t *testing.T) {
	definition := oneCRD(t)
	if err := applier.Validate(t.Context(), migration(t, installed(definition)), subjectFor(definition)); err != nil {
		t.Errorf("Validate refused a CRD whose stored version it declares: %v", err)
	}
}

func TestApplyCRDs_VerifyRefusesOneTheAPIServerHasNotEstablished(t *testing.T) {
	definition := oneCRD(t)
	live := installed(definition)
	live.Status.Conditions = nil

	err := applier.Verify(t.Context(), migration(t, live), subjectFor(definition))
	if err == nil {
		t.Fatal("Verify accepted a CRD with no conditions on it")
	}
	if !strings.Contains(err.Error(), string(apiextensionsv1.Established)) {
		t.Errorf("error is %q, want it to name the condition", err)
	}
}

func TestApplyCRDs_VerifyRefusesAVersionStoredAndNoLongerServed(t *testing.T) {
	definition := oneCRD(t)
	live := installed(definition)
	live.Status.StoredVersions = append(live.Status.StoredVersions, "v1alpha0")

	err := applier.Verify(t.Context(), migration(t, live), subjectFor(definition))
	if err == nil {
		t.Fatal("Verify accepted a CRD holding objects at a version it does not serve")
	}
	if !strings.Contains(err.Error(), "v1alpha0") {
		t.Errorf("error is %q, want it to name the version", err)
	}
}

func TestApplyCRDs_VerifyAcceptsAnEstablishedCRD(t *testing.T) {
	definition := oneCRD(t)
	if err := applier.Verify(t.Context(), migration(t, installed(definition)), subjectFor(definition)); err != nil {
		t.Errorf("Verify refused an established CRD: %v", err)
	}
}

func TestApplyCRDs_IgnoresASubjectThatIsNotItsOwn(t *testing.T) {
	// The runner hands a step the run's subjects when the step does not
	// enumerate, and a step that assumed its own would panic on the upgrade.
	scope := migration(t)

	for _, subject := range []upgrade.Subject{
		upgrade.TheUpgrade("simplyblock"),
		{Ref: upgrade.ObjectRef{GVK: crdGVK(), Name: "widgets.example.com"}},
	} {
		action, err := applier.Describe(t.Context(), scope, subject)
		if err != nil {
			t.Fatalf("Describe(%s): %v", subject, err)
		}
		if action != nil {
			t.Errorf("described %s for %s", action.Verb, subject)
		}
	}
}

func TestVerifyCRDVersions_WaitsForOneThatIsNotEstablished(t *testing.T) {
	definition := oneCRD(t)
	live := installed(definition)
	live.Status.Conditions = nil

	action, err := verifier.Describe(t.Context(), migration(t, live), subjectFor(definition))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if action == nil {
		t.Fatal("described nothing for a CRD the API server has not established")
	}
	if action.Verb != upgrade.VerbAwait {
		t.Errorf("verb = %s, want AWAIT", action.Verb)
	}
}

func TestVerifyCRDVersions_HasNothingToSayAboutAnEstablishedCRD(t *testing.T) {
	definition := oneCRD(t)
	scope := migration(t, installed(definition))
	subject := subjectFor(definition)

	action, err := verifier.Describe(t.Context(), scope, subject)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if action != nil {
		t.Errorf("described %s for an established CRD", action.Verb)
	}

	done, err := verifier.Done(t.Context(), scope, subject)
	if err != nil {
		t.Fatalf("Done: %v", err)
	}
	if !done {
		t.Error("an established CRD is not finished")
	}
}

func TestCarryOverCABundle_KeepsWhatTheClusterInjected(t *testing.T) {
	// The bundle is written by whatever issues the webhook's certificate, so a
	// CRD written straight from the generated file has none. Losing it breaks
	// conversion, which on a converting CRD is every read of the kind.
	withBundle := func(bundle []byte) *apiextensionsv1.CustomResourceDefinition {
		return &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Conversion: &apiextensionsv1.CustomResourceConversion{
					Strategy: apiextensionsv1.WebhookConverter,
					Webhook: &apiextensionsv1.WebhookConversion{
						ClientConfig: &apiextensionsv1.WebhookClientConfig{CABundle: bundle},
					},
				},
			},
		}
	}

	live, write := withBundle([]byte("injected")), withBundle(nil)
	carryOverCABundle(live, write)
	if got := string(write.Spec.Conversion.Webhook.ClientConfig.CABundle); got != "injected" {
		t.Errorf("bundle = %q, want the injected one", got)
	}

	// One the file carries itself wins, since that is a bundle somebody put
	// there on purpose.
	live, write = withBundle([]byte("injected")), withBundle([]byte("declared"))
	carryOverCABundle(live, write)
	if got := string(write.Spec.Conversion.Webhook.ClientConfig.CABundle); got != "declared" {
		t.Errorf("bundle = %q, want the declared one", got)
	}

	// A CRD that converts by any other strategy has no bundle to carry, and
	// reaching for one would be a nil dereference on every non-converting CRD.
	carryOverCABundle(
		&apiextensionsv1.CustomResourceDefinition{},
		&apiextensionsv1.CustomResourceDefinition{},
	)
}
