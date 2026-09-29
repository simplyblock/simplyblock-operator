// What an installation's stated cluster layout does to the document a discovery
// run writes.
//
// The seed exists for the fields that are immutable on the StorageCluster a draft
// expands into: a reviewer can correct anything else after the fact, and nobody
// can correct these. It applies to the one run the operator raised for this
// installation and to no other, which is the half of this the label carries and
// the half every case below is about.

package deployment

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/bootstrap"
	discoverypkg "github.com/simplyblock/simplyblock-operator/internal/discovery"
)

// statedLayout is an installation that decided the things discovery can only
// guess at.
func statedLayout() *bootstrap.Config {
	return &bootstrap.Config{
		Draft: bootstrap.DraftConfig{
			EdgeCluster: ptr.To(true),
			Images: bootstrap.ImagesConfig{
				SPDK:      "quay.io/simplyblock-io/spdk:v26.2.6",
				SPDKProxy: "quay.io/simplyblock-io/spdk-proxy:v26.2.6",
			},
			Cluster: bootstrap.ClusterConfig{
				Name: "fleet-cluster",
				Stripe: &simplyblockv1alpha2.StripeSpec{
					DataChunks:   ptr.To(int32(4)),
					ParityChunks: ptr.To(int32(2)),
				},
				MaxSubsystemCount:        ptr.To(int32(50)),
				EnableDriveFormat:        ptr.To(false),
				EnableChecksumValidation: ptr.To(true),
				EnableAtomicity4K:        ptr.To(true),
			},
		},
	}
}

// initialRun is the run the operator raised for this installation, marked as such.
func initialRun() *simplyblockv1alpha2.OperatorOps {
	return &simplyblockv1alpha2.OperatorOps{ObjectMeta: metav1.ObjectMeta{
		Name:   "initial-discovery",
		Labels: map[string]string{InitialDiscoveryLabel: "true"},
	}}
}

func noteMentioning(notes []string, want string) bool {
	for _, note := range notes {
		if strings.Contains(note, want) {
			return true
		}
	}
	return false
}

// The stated layout is what the draft proposes, because every member of it is
// immutable on the cluster and a reviewer approving a guess cannot undo it.
func TestTheStatedLayoutSeedsTheInitialRunsDraft(t *testing.T) {
	r := &OperatorOpsReconciler{}

	draft, notes := r.draftFor(initialRun(), &simplyblockv1alpha2.DiscoverSpec{},
		discoverypkg.Plan{}, statedLayout())

	cluster := draft.Spec.Cluster
	if cluster == nil {
		t.Fatal("the draft proposes no cluster")
	}
	if cluster.Name != "fleet-cluster" {
		t.Errorf("cluster name = %q, want the stated one", cluster.Name)
	}
	if cluster.Stripe == nil ||
		ptr.IntFrom(cluster.Stripe.DataChunks, 0) != 4 ||
		ptr.IntFrom(cluster.Stripe.ParityChunks, 0) != 2 {
		t.Errorf("stripe = %+v, want the stated 4+2 rather than one derived from fleet size",
			cluster.Stripe)
	}
	if ptr.IntFrom(cluster.MaxSubsystemCount, 0) != 50 {
		t.Errorf("maxSubsystemCount = %v, want the stated 50", cluster.MaxSubsystemCount)
	}
	if cluster.EnableDriveFormat == nil || *cluster.EnableDriveFormat {
		t.Errorf("enableDriveFormat = %v, want the stated refusal: discovery's own "+
			"default formats every drive the draft names", cluster.EnableDriveFormat)
	}
	if !ptr.BoolFromOrFalse(cluster.EnableChecksumValidation) {
		t.Error("enableChecksumValidation was dropped; a cluster created without it " +
			"is one nobody can turn it on for")
	}
	if !ptr.BoolFromOrFalse(cluster.EnableAtomicity4K) {
		t.Error("enableAtomicity4K was dropped")
	}

	// A number a reviewer cannot account for is a number they cannot correct with
	// confidence, so a value that was stated has to read as stated rather than as
	// something the run concluded from the fleet.
	if !noteMentioning(notes, "stated") {
		t.Errorf("the notes are %v, and none of them says the layout was stated", notes)
	}
}

// The document's own fields, which discovery never sets and nothing about a
// worker says.
func TestTheStatedDraftFieldsReachTheDocument(t *testing.T) {
	r := &OperatorOpsReconciler{}

	draft, _ := r.draftFor(initialRun(), &simplyblockv1alpha2.DiscoverSpec{},
		discoverypkg.Plan{}, statedLayout())

	if !ptr.BoolFromOrFalse(draft.Spec.EdgeCluster) {
		t.Error("edgeCluster was dropped")
	}
	if draft.Spec.Images == nil {
		t.Fatal("the stated images did not reach the draft")
	}
	if draft.Spec.Images.SPDK == nil || draft.Spec.Images.SPDK.Image != "quay.io/simplyblock-io/spdk:v26.2.6" {
		t.Errorf("spdk = %+v, want the stated reference", draft.Spec.Images.SPDK)
	}
	if draft.Spec.Images.SPDKProxy == nil ||
		draft.Spec.Images.SPDKProxy.Image != "quay.io/simplyblock-io/spdk-proxy:v26.2.6" {
		t.Errorf("spdkProxy = %+v, want the stated reference", draft.Spec.Images.SPDKProxy)
	}
	// The node agent is not seeded: a draft that states none takes the
	// ControlPlane singleton's, which the chart already writes.
	if draft.Spec.Images.NodeAgent != nil {
		t.Errorf("nodeAgent = %+v, want the ControlPlane's own to decide", draft.Spec.Images.NodeAgent)
	}
}

// A run somebody wrote is not the run this installation was configured for, and
// its draft is discovery's own proposal.
//
// Without this, a second discovery run months later would silently re-apply a
// layout stated at install time, over a fleet that has since changed.
func TestARunNobodyLabeledGetsNoSeed(t *testing.T) {
	r := &OperatorOpsReconciler{}
	theirs := &simplyblockv1alpha2.OperatorOps{
		ObjectMeta: metav1.ObjectMeta{Name: "discover-again"},
	}

	draft, _ := r.draftFor(theirs, &simplyblockv1alpha2.DiscoverSpec{},
		discoverypkg.Plan{}, statedLayout())

	cluster := draft.Spec.Cluster
	if cluster == nil {
		t.Fatal("the draft proposes no cluster")
	}
	if cluster.Name == "fleet-cluster" {
		t.Error("a hand-written run was given the installation's stated cluster name")
	}
	if ptr.IntFrom(cluster.MaxSubsystemCount, 0) != int(discoverypkg.DefaultMaxSubsystemCount) {
		t.Errorf("maxSubsystemCount = %v, want discovery's own %d",
			cluster.MaxSubsystemCount, discoverypkg.DefaultMaxSubsystemCount)
	}
	if !ptr.BoolFromOrFalse(cluster.EnableDriveFormat) {
		t.Error("a hand-written run was given the installation's refusal to format drives")
	}
	if cluster.EnableChecksumValidation != nil {
		t.Errorf("enableChecksumValidation = %v, want the cluster's own default to decide",
			cluster.EnableChecksumValidation)
	}
	if draft.Spec.EdgeCluster != nil {
		t.Error("a hand-written run's draft was called an edge deployment")
	}
	if draft.Spec.Images != nil {
		t.Errorf("images = %+v, want a hand-written run's draft to pin none", draft.Spec.Images)
	}
}

// A field the installation left alone stays discovery's, which is the whole point
// of the seed being partial: an installation states what it decided and nothing
// more.
func TestAnUnstatedFieldStaysDerived(t *testing.T) {
	r := &OperatorOpsReconciler{}
	partial := &bootstrap.Config{Draft: bootstrap.DraftConfig{
		Cluster: bootstrap.ClusterConfig{EnableChecksumValidation: ptr.To(true)},
	}}

	draft, _ := r.draftFor(initialRun(), &simplyblockv1alpha2.DiscoverSpec{},
		discoverypkg.Plan{}, partial)

	cluster := draft.Spec.Cluster
	if !ptr.BoolFromOrFalse(cluster.EnableChecksumValidation) {
		t.Fatal("the one stated field was dropped")
	}
	if ptr.IntFrom(cluster.MaxSubsystemCount, 0) != int(discoverypkg.DefaultMaxSubsystemCount) {
		t.Errorf("maxSubsystemCount = %v, want discovery's own %d",
			cluster.MaxSubsystemCount, discoverypkg.DefaultMaxSubsystemCount)
	}
	if !ptr.BoolFromOrFalse(cluster.EnableDriveFormat) {
		t.Error("an unstated enableDriveFormat lost discovery's own default")
	}
	if cluster.Stripe == nil {
		t.Error("an unstated stripe lost the layout discovery derives from fleet size")
	}
	if cluster.Name == "" {
		t.Error("an unstated cluster name left the draft proposing a cluster with no name")
	}
}

// A growth document names a cluster rather than describing one, so there is no
// layout for a seed to apply to and stating one must not invent a template
// admission would refuse beside a clusterRef.
func TestAGrowthDraftIsNotSeeded(t *testing.T) {
	r := &OperatorOpsReconciler{}
	spec := &simplyblockv1alpha2.DiscoverSpec{ClusterRef: "simplyblock-cluster"}

	draft, _ := r.draftFor(initialRun(), spec, discoverypkg.Plan{}, statedLayout())

	if draft.Spec.Cluster != nil {
		t.Errorf("a growth document proposes the layout %+v beside its clusterRef",
			draft.Spec.Cluster)
	}
	if draft.Spec.ClusterRef != "simplyblock-cluster" {
		t.Errorf("clusterRef = %q, want the cluster the run names", draft.Spec.ClusterRef)
	}
}
