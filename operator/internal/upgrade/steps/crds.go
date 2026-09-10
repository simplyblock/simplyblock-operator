// §11's CRD installation, and the check that the API server took all of it.
//
// The set is the one embedded in this binary, so that the CRDs a run installs
// are the ones the conversion code in the same binary was built against. That
// is also why the step enumerates its own subjects rather than reading the
// graph: a kind that is new in v1alpha2 has no CRD in the cluster to discover,
// and it is exactly the CRD this step exists to create.
//
// A CRD whose content already matches is not written. §11 says so directly, and
// the reason is that a CRD is the one object in the cluster every custom
// resource is served through: a write that changes nothing can still fail, and
// a failed write here takes the API for every kind with it.

package steps

import (
	"context"
	"fmt"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/crds"
)

// crdGVK is what a CRD subject reports.
func crdGVK() schema.GroupVersionKind {
	return apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition")
}

// applyCRDs writes the embedded CRDs, one subject per CRD.
type applyCRDs struct{}

func (applyCRDs) ID() upgrade.ID {
	return IDApplyCRDs
}
func (applyCRDs) Stage() upgrade.Stage {
	return upgrade.StageUpgrade
}
func (applyCRDs) Phase() upgrade.Phase {
	return ""
}
func (applyCRDs) Requires() []upgrade.ID {
	return []upgrade.ID{IDSmokeTestWebhook}
}
func (applyCRDs) Description() string {
	return "applies the CRDs this binary carries, so the schemas installed are the ones its conversion code was built against"
}

// Subjects is one per embedded CRD, in the order Load returns them.
//
// The object carried is the CRD as it is to be written rather than one read
// from the cluster, because half of them may not be installed. What is in the
// cluster is read by the step, against the subject it was handed.
func (applyCRDs) Subjects(_ context.Context, _ *upgrade.Scope) ([]upgrade.Subject, error) {
	loaded, err := crds.Load()
	if err != nil {
		return nil, err
	}

	out := make([]upgrade.Subject, 0, len(loaded))
	for _, definition := range loaded {
		out = append(out, subjectFor(definition))
	}
	return out, nil
}

// subjectFor names one embedded CRD. A CRD is cluster-scoped, so the reference
// carries no namespace, and it carries no UID either: the CRD it names need not
// exist, which is the case that makes this step a create.
func subjectFor(definition crds.Definition) upgrade.Subject {
	return upgrade.Subject{
		Ref:    upgrade.ObjectRef{GVK: crdGVK(), Name: definition.Name()},
		Object: definition.Object,
	}
}

// Describe reports what this CRD needs, and nothing when it is already the one
// this binary would write.
func (a applyCRDs) Describe(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) (*upgrade.Action, error) {
	desired, live, err := a.read(ctx, s, subject)
	if err != nil || desired == nil {
		return nil, err
	}

	if live == nil {
		return &upgrade.Action{
			Rule:   IDApplyCRDs,
			Verb:   upgrade.VerbCreate,
			Object: subject.Ref,
			Detail: served(desired.Object),
		}, nil
	}
	if desired.Matches(live) {
		return nil, nil
	}

	return &upgrade.Action{
		Rule:   IDApplyCRDs,
		Verb:   upgrade.VerbUpdate,
		Object: subject.Ref,
		Detail: difference(desired, live),
	}, nil
}

// difference says what an update changes, in the form a plan line prints.
//
// The versions when they move, since that is what §11 is about and what a
// reader is checking for. Otherwise, the schema moved and the versions did not,
// and printing the versions with an arrow between two identical halves would
// say a change that is not the one being made.
func difference(desired *crds.Definition, live *apiextensionsv1.CustomResourceDefinition) string {
	if from, to := served(live), served(desired.Object); from != to {
		return from + " → " + to
	}
	return "schema, at " + served(live)
}

// Done claims a CRD the cluster already holds as this binary describes it.
func (a applyCRDs) Done(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) (bool, error) {
	desired, live, err := a.read(ctx, s, subject)
	if err != nil || desired == nil {
		return false, err
	}
	return desired.Matches(live), nil
}

// Validate refuses a write that would take a version away from objects that are
// still stored in it.
//
// The API server refuses it too, and later: it accepts the CRD and then fails
// every read of the kind. Catching it here names the version and the kind
// instead, and §24's storage-version rewrite is what resolves it.
func (a applyCRDs) Validate(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	desired, live, err := a.read(ctx, s, subject)
	if err != nil || desired == nil || live == nil {
		return err
	}

	declared := make(map[string]bool, len(desired.Object.Spec.Versions))
	for _, version := range desired.Object.Spec.Versions {
		declared[version.Name] = true
	}

	var dropped []string
	for _, stored := range live.Status.StoredVersions {
		if !declared[stored] {
			dropped = append(dropped, stored)
		}
	}
	if len(dropped) > 0 {
		return fmt.Errorf(
			"%s still stores objects at %s, and the CRD this binary carries does not declare it: "+
				"the stored versions have to be rewritten (§24) before the version can go",
			live.Name, strings.Join(dropped, ", "))
	}
	return nil
}

// Apply writes the CRD.
//
// An update carries the live object's resourceVersion rather than being a blind
// write, so a CRD something else changed between the plan and the apply fails
// on the conflict instead of being overwritten by it.
func (a applyCRDs) Apply(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	desired, live, err := a.read(ctx, s, subject)
	if err != nil || desired == nil {
		return err
	}

	write := desired.Object.DeepCopy()
	if live == nil {
		if err := s.Client.Create(ctx, write); err != nil {
			return fmt.Errorf("creating %s: %w", write.Name, err)
		}
		return nil
	}

	write.ResourceVersion = live.ResourceVersion
	carryOverCABundle(live, write)
	if err := s.Client.Update(ctx, write); err != nil {
		return fmt.Errorf("updating %s: %w", write.Name, err)
	}
	return nil
}

// Verify reads the CRD back and holds it to §11's expectation.
func (a applyCRDs) Verify(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	desired, live, err := a.read(ctx, s, subject)
	if err != nil {
		return err
	}
	if desired == nil {
		return nil
	}
	if live == nil {
		return fmt.Errorf("%s is not there after being applied", subject.Ref.Name)
	}
	if !desired.Matches(live) {
		return fmt.Errorf("%s is installed as something other than the CRD this binary carries", live.Name)
	}
	return accepted(desired, live)
}

// read resolves the subject into the CRD this binary carries and the one the
// cluster has, either of which may be absent.
//
// A subject that is not one of this step's is not an error: the runner may hand
// a step the run's subjects, and a step that panicked on one would be a step
// that depended on the runner never doing so.
func (applyCRDs) read(
	ctx context.Context, s *upgrade.Scope, subject upgrade.Subject,
) (*crds.Definition, *apiextensionsv1.CustomResourceDefinition, error) {
	if subject.IsUpgrade() || subject.Ref.GVK != crdGVK() {
		return nil, nil, nil
	}

	loaded, err := crds.Load()
	if err != nil {
		return nil, nil, err
	}

	var desired *crds.Definition
	for i := range loaded {
		if loaded[i].Name() == subject.Ref.Name {
			desired = &loaded[i]
			break
		}
	}
	if desired == nil {
		return nil, nil, nil
	}

	var live apiextensionsv1.CustomResourceDefinition
	switch err := s.Client.Get(ctx, types.NamespacedName{Name: subject.Ref.Name}, &live); {
	case apierrors.IsNotFound(err):
		return desired, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("reading %s: %w", subject.Ref.Name, err)
	}
	return desired, &live, nil
}

// accepted holds one installed CRD to what §11 expects of it: the API server
// established it, took its names, and serves what the file declares.
//
// The expectation is read from the CRD rather than from the group it is in, so
// a converting CRD is checked against both of its versions and a single-version
// one is not checked against a version it never had.
func accepted(desired *crds.Definition, live *apiextensionsv1.CustomResourceDefinition) error {
	for _, want := range []apiextensionsv1.CustomResourceDefinitionConditionType{
		apiextensionsv1.Established, apiextensionsv1.NamesAccepted,
	} {
		if !condition(live, want) {
			return fmt.Errorf("%s is not %s, so the API server has not accepted it", live.Name, want)
		}
	}

	if got, want := served(live), served(desired.Object); got != want {
		return fmt.Errorf("%s serves %s, and this binary's CRD declares %s", live.Name, got, want)
	}

	storage, named := desired.Storage()
	if !named {
		return fmt.Errorf("the embedded %s names no storage version", desired.Source)
	}
	for _, version := range live.Spec.Versions {
		if version.Storage && version.Name != storage {
			return fmt.Errorf("%s stores at %s, and this binary's CRD stores at %s",
				live.Name, version.Name, storage)
		}
	}

	// §11 wants the stored versions checked too, because that is what says
	// whether the objects already in etcd can still be read. A version that is
	// stored and no longer served is the state that breaks every read of the
	// kind.
	declared := make(map[string]bool, len(live.Spec.Versions))
	for _, version := range live.Spec.Versions {
		declared[version.Name] = version.Served
	}
	for _, stored := range live.Status.StoredVersions {
		if !declared[stored] {
			return fmt.Errorf("%s holds objects stored at %s, which it no longer serves", live.Name, stored)
		}
	}
	return nil
}

// condition reports whether the CRD carries one condition as true.
func condition(
	live *apiextensionsv1.CustomResourceDefinition, want apiextensionsv1.CustomResourceDefinitionConditionType,
) bool {
	for _, held := range live.Status.Conditions {
		if held.Type == want {
			return held.Status == apiextensionsv1.ConditionTrue
		}
	}
	return false
}

// served renders a CRD's served versions and which of them is stored, in the
// form a plan line prints.
func served(crd *apiextensionsv1.CustomResourceDefinition) string {
	parts := make([]string, 0, len(crd.Spec.Versions))
	for _, version := range crd.Spec.Versions {
		if !version.Served {
			continue
		}
		if version.Storage {
			parts = append(parts, version.Name+" (stored)")
			continue
		}
		parts = append(parts, version.Name)
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

// carryOverCABundle keeps the CA the cluster injected into the CRD's conversion
// webhook configuration.
//
// The bundle is written by whatever issues the webhook's certificate rather
// than by the generated file, so a CRD written straight from the file has an
// empty one. Conversion then fails for every object of the kind, which on a
// converting CRD is every read of it.
func carryOverCABundle(live, write *apiextensionsv1.CustomResourceDefinition) {
	from := webhookClientConfig(live)
	into := webhookClientConfig(write)
	if from == nil || into == nil || len(from.CABundle) == 0 || len(into.CABundle) > 0 {
		return
	}
	into.CABundle = from.CABundle
}

// webhookClientConfig reaches the conversion webhook's client configuration, or
// nil where the CRD converts by any other strategy.
func webhookClientConfig(crd *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.WebhookClientConfig {
	if crd.Spec.Conversion == nil || crd.Spec.Conversion.Webhook == nil {
		return nil
	}
	return crd.Spec.Conversion.Webhook.ClientConfig
}

// establishTimeout is how long a CRD is given to become established.
//
// Establishment is the API server accepting the names and starting to serve the
// kind, which is fast and local to the API server. A minute is generous for
// that, and it is short enough that a set which is never going to establish
// fails the upgrade rather than hanging it.
const establishTimeout = time.Minute

// verifyCRDVersions waits for the applied set and holds all of it to §11.
//
// It is a separate step from applying because §11's requirement is about the
// set rather than about a CRD: the state that must not be proceeded from is a
// partially applied one, where the operator reconciles one kind at v1alpha2 and
// another at v1alpha1. Applying verifies each CRD as it is written, and this is
// what refuses to go on when any of them did not take.
type verifyCRDVersions struct{ applyCRDs }

func (verifyCRDVersions) ID() upgrade.ID {
	return IDVerifyCRDVersions
}
func (verifyCRDVersions) Requires() []upgrade.ID {
	return []upgrade.ID{IDApplyCRDs}
}

func (verifyCRDVersions) Description() string {
	return "waits for every applied CRD and checks the API server took all of them, since a partly applied set is the worst outcome"
}

// Describe reports a CRD that is not yet what §11 expects, and nothing for one
// that already is.
func (v verifyCRDVersions) Describe(
	ctx context.Context, s *upgrade.Scope, subject upgrade.Subject,
) (*upgrade.Action, error) {
	ready, err := v.Done(ctx, s, subject)
	if err != nil || ready {
		return nil, err
	}

	desired, _, err := v.read(ctx, s, subject)
	if err != nil || desired == nil {
		return nil, err
	}
	return &upgrade.Action{
		Rule:   IDVerifyCRDVersions,
		Verb:   upgrade.VerbAwait,
		Object: subject.Ref,
		Detail: served(desired.Object),
	}, nil
}

// Done claims a CRD the API server has established and is serving as declared.
func (v verifyCRDVersions) Done(
	ctx context.Context, s *upgrade.Scope, subject upgrade.Subject,
) (bool, error) {
	desired, live, err := v.read(ctx, s, subject)
	if err != nil || desired == nil || live == nil {
		return false, err
	}
	return accepted(desired, live) == nil, nil
}

// Validate has nothing to refuse. Waiting for a CRD to be established has no
// precondition beyond the CRD existing, which the wait itself reports.
func (verifyCRDVersions) Validate(context.Context, *upgrade.Scope, upgrade.Subject) error {
	return nil
}

// Apply waits, which is the whole of what this step does to the cluster:
// nothing. §11 requires the wait, and a check that ran the instant after a
// write would fail on the API server not having caught up rather than on
// anything being wrong.
func (v verifyCRDVersions) Apply(ctx context.Context, s *upgrade.Scope, subject upgrade.Subject) error {
	var last error
	err := wait.PollUntilContextTimeout(ctx, establishInterval, establishTimeout, true,
		func(ctx context.Context) (bool, error) {
			desired, live, err := v.read(ctx, s, subject)
			if err != nil {
				return false, err
			}
			if desired == nil {
				return true, nil
			}
			if live == nil {
				last = fmt.Errorf("%s is not installed", subject.Ref.Name)
				return false, nil
			}
			if last = accepted(desired, live); last != nil {
				return false, nil
			}
			return true, nil
		})

	if err != nil && last != nil {
		return fmt.Errorf("%s did not become ready within %s: %w", subject.Ref.Name, establishTimeout, last)
	}
	return err
}

// establishInterval is how often the wait looks.
const establishInterval = time.Second
