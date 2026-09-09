// Where a cluster is in the upgrade, read from the cluster rather than passed
// by the user.
//
// §27 requires it: the one read-only command reports the plan for whichever
// phase the cluster is positioned for, and it knows which by reading, so
// nobody has to tell it. The signal is §7.4's staging. The seven converting
// kinds serve v1alpha1 alone until the upgrade applies the new CRDs, and both
// versions afterward, so what the API server serves is what the upgrade has
// already done.

package upgrade

import (
	"context"
	"fmt"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// The two API versions this migration carries a cluster between.
const (
	VersionOld = "v1alpha1"
	VersionNew = "v1alpha2"
)

// ConvertingKind is one of §7.2's seven: a kind the redesign renames or removes
// a property of, which therefore serves both versions behind a conversion.
type ConvertingKind struct {
	// Kind is the kind's name.
	Kind string

	// Plural is what its CRD is named for, which is how the CRD is looked up.
	Plural string
}

// CRDName is the CustomResourceDefinition that defines this kind.
func (c ConvertingKind) CRDName() string { return c.Plural + "." + APIGroup }

// ConvertingKinds are the seven of §7.2. Ten of the seventeen registered kinds
// are absent: five keep one version and are then removed, and five are outside
// the redesign, so neither group has a second version to convert between.
//
// It is data rather than prose because three things read it: the positioning
// below, the storage rewrite of §24, and the coverage of the migration.
func ConvertingKinds() []ConvertingKind {
	return []ConvertingKind{
		{Kind: "StorageCluster", Plural: "storageclusters"},
		{Kind: "StorageClusterOps", Plural: "storageclusterops"},
		{Kind: "StorageNode", Plural: "storagenodes"},
		{Kind: "StorageNodeOps", Plural: "storagenodeops"},
		{Kind: "StoragePool", Plural: "storagepools"},
		{Kind: "ControlPlane", Plural: "controlplanes"},
		{Kind: "StorageBackup", Plural: "storagebackups"},
	}
}

// Position is where a cluster stands, and why.
type Position struct {
	// Stage is the command the cluster is positioned for.
	Stage Stage

	// Because is the sentence a report prints under it.
	Because string

	// Converted are the kinds already serving the new version, and Pending the
	// ones not. A cluster with both is a partially applied CRD set, which §11
	// refuses to proceed past.
	Converted []string
	Pending   []string
}

// Partial reports a CRD set that was applied to some kinds and not others,
// which leaves the operator reconciling one kind at each version.
func (p Position) Partial() bool { return len(p.Converted) > 0 && len(p.Pending) > 0 }

// Positioned reads the cluster and reports which command it is ready for.
//
// A kind whose CRD is absent counts as pending rather than as an error. The
// group not being installed at all is a prerequisite failure with its own
// report (§9.2), and answering "not upgraded yet" here is both true and the
// safe direction to be wrong in.
func Positioned(ctx context.Context, s *Scope) (Position, error) {
	var position Position

	for _, kind := range ConvertingKinds() {
		var crd apiextensionsv1.CustomResourceDefinition
		err := s.Client.Get(ctx, types.NamespacedName{Name: kind.CRDName()}, &crd)
		switch {
		case apierrors.IsNotFound(err):
			position.Pending = append(position.Pending, kind.Kind)
			continue
		case err != nil:
			return position, fmt.Errorf("reading the CRD for %s: %w", kind.Kind, err)
		}

		if serves(crd, VersionNew) {
			position.Converted = append(position.Converted, kind.Kind)
			continue
		}
		position.Pending = append(position.Pending, kind.Kind)
	}

	sort.Strings(position.Converted)
	sort.Strings(position.Pending)

	switch {
	case len(position.Pending) == 0:
		position.Stage = StageMigrate
		position.Because = fmt.Sprintf(
			"every one of the %d converting kinds serves %s, so the API upgrade is done and the resource model is not",
			len(position.Converted), VersionNew)
	case position.Partial():
		// Positioned for the upgrade, because that is the command that would
		// finish applying the set.
		position.Stage = StageUpgrade
		position.Because = fmt.Sprintf(
			"%d of the %d converting kinds serve %s and %d do not, which leaves the operator reconciling one kind at each version",
			len(position.Converted), len(position.Converted)+len(position.Pending), VersionNew, len(position.Pending))
	default:
		position.Stage = StageUpgrade
		position.Because = fmt.Sprintf(
			"no converting kind serves %s yet, so the cluster cannot run an operator that reads it",
			VersionNew)
	}
	return position, nil
}

// serves reports whether a CRD serves this version.
func serves(crd apiextensionsv1.CustomResourceDefinition, version string) bool {
	for _, served := range crd.Spec.Versions {
		if served.Name == version && served.Served {
			return true
		}
	}
	return false
}
