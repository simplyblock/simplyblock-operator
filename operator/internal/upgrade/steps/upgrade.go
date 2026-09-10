// §9.1's sequence, described in full and performed in part.
//
// The steps are here before their implementations because the plan is what a
// user reads to decide whether to run the thing, and a plan short of what the
// upgrade owes is worse than no plan: silence reads as nothing to do. So each
// step describes its work now, says what is missing where anything is, and the
// runner refuses the stage rather than performing the part that exists and
// stopping in the middle.
//
// The two CRD steps are in crds.go and are real. What the rest are missing is
// mostly one thing: §29.1's v1alpha2 package covers two of the eleven new
// kinds and none of the seven converting ones, and §29.2's conversion
// functions do not exist, so there is nothing to convert between and nothing
// to deploy a conversion webhook for. The release handover is blocked on a
// different thing, which is the new chart not being rendered yet.
//
// Step 1 of §9.1 is absent on purpose: validating prerequisites is the Check
// registry, which already runs for this stage. Step 13 is absent too, being the
// command's closing report rather than a change to the cluster.

package steps

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// The identities of §9.1's steps.
const (
	IDDeployWebhook     upgrade.ID = "deploy-conversion-webhook"
	IDAwaitWebhook      upgrade.ID = "await-conversion-webhook"
	IDSmokeTestWebhook  upgrade.ID = "smoke-test-conversion"
	IDApplyCRDs         upgrade.ID = "apply-crds"
	IDVerifyCRDVersions upgrade.ID = "verify-crd-versions"
	IDRetestConversion  upgrade.ID = "retest-conversion"
	IDHandOverRelease   upgrade.ID = "hand-over-helm-release"
	IDUpgradeOperator   upgrade.ID = "upgrade-operator"
	IDAwaitOperator     upgrade.ID = "await-operator"
	IDSmokeTestOperator upgrade.ID = "smoke-test-operator"
)

// What each group of steps waits on, said once so the reasons do not drift.
const (
	needsConversion  = "the conversion webhook of §29.3 and the conversion functions of §29.2 do not exist"
	needsV1Alpha2    = "the v1alpha2 types of §29.1 cover two of the eleven new kinds and none of the seven converting ones"
	needsHelmSDK     = "Helm's Go SDK is not a dependency yet (§29.4), and the release is upgraded through it"
	needsChartRender = "classifying an object as a survivor needs the new chart rendered, which is Helm's Go SDK (§29.4); " +
		"reading the deployed release needs nothing, so the objects below are the real ones"
)

// Upgrade returns §9.1's sequence.
func Upgrade() []upgrade.Step {
	return []upgrade.Step{
		planned{
			id:      IDDeployWebhook,
			summary: "deploys the conversion webhook, which runs from the operator's image and not from the operator",
			verb:    upgrade.VerbCreate,
			blocked: needsConversion,
		},
		planned{
			id:      IDAwaitWebhook,
			summary: "waits until the conversion webhook is serving and its CA bundle has reached the CRDs",
			verb:    upgrade.VerbAwait,
			blocked: needsConversion,
			needs:   []upgrade.ID{IDDeployWebhook},
		},
		planned{
			id:      IDSmokeTestWebhook,
			summary: "converts a real resource, because Pod readiness does not prove that conversion works",
			verb:    upgrade.VerbVerify,
			blocked: needsConversion,
			needs:   []upgrade.ID{IDAwaitWebhook},
		},
		applyCRDs{},
		verifyCRDVersions{},
		planned{
			id:      IDRetestConversion,
			summary: "converts again now that the CRDs have changed under the webhook",
			verb:    upgrade.VerbVerify,
			blocked: needsConversion,
			needs:   []upgrade.ID{IDVerifyCRDVersions},
		},
		handOverRelease{described: described{id: IDHandOverRelease, blocked: needsChartRender}},
		planned{
			id:      IDUpgradeOperator,
			summary: "upgrades the operator, translating the deployed release's values into the new chart's spellings",
			verb:    upgrade.VerbUpdate,
			blocked: needsHelmSDK,
			needs:   []upgrade.ID{IDHandOverRelease},
		},
		planned{
			id:      IDAwaitOperator,
			summary: "waits for the new operator, and for it to adopt what the release handed over",
			verb:    upgrade.VerbAwait,
			blocked: needsHelmSDK,
			needs:   []upgrade.ID{IDUpgradeOperator},
		},
		planned{
			id:      IDSmokeTestOperator,
			summary: "writes a v1alpha2 resource, because a read proves conversion and only a write proves admission",
			verb:    upgrade.VerbVerify,
			blocked: needsV1Alpha2,
			needs:   []upgrade.ID{IDAwaitOperator},
		},
	}
}

// planned is a step that describes its work and cannot yet perform it.
//
// Every one of §9.1's acts on the upgrade rather than on anything in the
// cluster, which is what [upgrade.Subject] exists for: deploying a webhook and
// upgrading an operator change the installation, and neither has an object in
// the graph to be about.
type planned struct {
	id      upgrade.ID
	summary string
	verb    upgrade.Verb
	blocked string
	needs   []upgrade.ID
}

func (p planned) ID() upgrade.ID         { return p.id }
func (p planned) Description() string    { return p.summary }
func (p planned) Stage() upgrade.Stage   { return upgrade.StageUpgrade }
func (p planned) Phase() upgrade.Phase   { return "" }
func (p planned) Requires() []upgrade.ID { return p.needs }
func (p planned) BlockedBy() string      { return p.blocked }

// Describe reports the work against the upgrade itself, and nothing against any
// other subject.
func (p planned) Describe(_ context.Context, _ *upgrade.Scope, subject upgrade.Subject) (*upgrade.Action, error) {
	if !subject.IsUpgrade() {
		return nil, nil
	}
	// No detail. These steps act on the upgrade rather than on an object, so
	// there is no old value and new value to print, and a sentence describing
	// the step would be its documentation printed on every run.
	return &upgrade.Action{Rule: p.id, Verb: p.verb, Object: subject.Ref}, nil
}

// Done is always false. Whether the work has already happened is a question its
// implementation will answer, and answering yes here would drop the step from
// the plan it exists to appear in.
func (p planned) Done(context.Context, *upgrade.Scope, upgrade.Subject) (bool, error) {
	return false, nil
}

// The three that perform. None of them is reached: the runner refuses a stage
// with a blocked step in it before applying anything, so these exist to make
// the failure explicit rather than silent if that ever stops being true.
func (p planned) Validate(context.Context, *upgrade.Scope, upgrade.Subject) error {
	return errNotImplemented(p)
}

func (p planned) Apply(context.Context, *upgrade.Scope, upgrade.Subject) error {
	return errNotImplemented(p)
}

func (p planned) Verify(context.Context, *upgrade.Scope, upgrade.Subject) error {
	return errNotImplemented(p)
}

// errNotImplemented is what a described step returns if it is ever asked to
// act, which would mean the runner's refusal had been bypassed.
func errNotImplemented(p planned) error {
	return fmt.Errorf("%s is described and not implemented: %s", p.id, p.blocked)
}

// The Helm metadata §12 reads and writes. helm.sh/resource-policy is read from
// the live object rather than from the stored release manifest, which is what
// makes annotating in the cluster enough, and meta.helm.sh/release-name is what
// marks an object as one a release installed.
const (
	annoResourcePolicy = "helm.sh/resource-policy"
	annoReleaseName    = "meta.helm.sh/release-name"

	policyKeep = "keep"
)

// handOverRelease annotates what the Helm release is about to stop containing.
//
// §12 is the riskiest step of the upgrade: the chart that carries the new
// operator carries the operator and nothing else, so the upgrade that installs
// it is also the upgrade that deletes the running data plane, and four of the
// prunes would end the upgrade rather than degrade the cluster.
//
// It describes per object because that is what it does per object. Which
// objects survive needs the new chart rendered, which is the half Helm's SDK
// owns, but which objects the release installed needs only the release, so the
// subjects below are the real ones rather than a placeholder.
type handOverRelease struct {
	described
}

func (h handOverRelease) ID() upgrade.ID     { return h.id }
func (handOverRelease) Stage() upgrade.Stage { return upgrade.StageUpgrade }
func (handOverRelease) Phase() upgrade.Phase { return "" }

func (handOverRelease) Requires() []upgrade.ID {
	return []upgrade.ID{IDVerifyCRDVersions}
}

func (handOverRelease) Description() string {
	return "annotates every object the Helm release installed that has to survive the upgrade that stops containing it"
}

// Describe names one object of the release.
//
// The operator's own objects are described too. Which of them the new chart
// still contains is the classification this step cannot yet make, and guessing
// it from a name would be the table §12.1 says the set is never read from.
func (h handOverRelease) Describe(_ context.Context, _ *upgrade.Scope, subject upgrade.Subject) (*upgrade.Action, error) {
	if subject.IsUpgrade() || subject.Object == nil {
		return nil, nil
	}
	if !installedByHelm(subject.Object) || alreadyKept(subject.Object) {
		return nil, nil
	}
	return &upgrade.Action{
		Rule:   h.id,
		Verb:   upgrade.VerbAnnotate,
		Object: subject.Ref,
		Detail: annoResourcePolicy + "=" + policyKeep + ", if the new chart no longer contains it",
	}, nil
}

// Done claims an object of the release that already carries the annotation.
func (handOverRelease) Done(_ context.Context, _ *upgrade.Scope, subject upgrade.Subject) (bool, error) {
	if subject.IsUpgrade() || subject.Object == nil {
		return false, nil
	}
	return installedByHelm(subject.Object) && alreadyKept(subject.Object), nil
}

// installedByHelm reports an object a Helm release owns, which is the metadata
// Helm writes on everything it installs and §12.3 removes once the operator has
// adopted it.
func installedByHelm(obj client.Object) bool {
	return obj.GetAnnotations()[annoReleaseName] != ""
}

// alreadyKept reports an object Helm will already refuse to prune.
func alreadyKept(obj client.Object) bool {
	return obj.GetAnnotations()[annoResourcePolicy] == policyKeep
}
