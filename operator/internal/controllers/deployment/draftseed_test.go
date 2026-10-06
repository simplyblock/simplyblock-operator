// What a run's stated seed does to the document a discovery run writes.
//
// The seed exists for the fields a wrong guess is expensive to undo, which is
// mostly the fields that are immutable on the StorageCluster a draft expands
// into and is not only those. maxSubsystemCount can be changed afterward and is
// seeded because the number discovery proposes is the middle of the API's range
// rather than a reading; enableDriveFormat is spent during provisioning rather
// than held as cluster state, and undoing it means restoring a backup.
//
// It applies to the run that states it and to no other, because it lives on that
// run's spec.discover.seed and nothing else carries it.

package deployment

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	discoverypkg "github.com/simplyblock/simplyblock-operator/internal/discovery"
)

// statedLayout is a run's seed: the things its author decided that discovery can
// only guess at.
func statedLayout() *simplyblockv1alpha2.DraftSeed {
	return &simplyblockv1alpha2.DraftSeed{
		EdgeCluster: ptr.To(true),
		Images: &simplyblockv1alpha2.DeploymentImages{
			SPDK:      &simplyblockv1alpha2.ImageSpec{Image: "quay.io/simplyblock-io/spdk:v26.2.6"},
			SPDKProxy: &simplyblockv1alpha2.ImageSpec{Image: "quay.io/simplyblock-io/spdk-proxy:v26.2.6"},
		},
		Cluster: &simplyblockv1alpha2.DraftSeedCluster{
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
	}
}

// seeded is a discovery spec stating the layout.
func seeded() *simplyblockv1alpha2.DiscoverSpec {
	return &simplyblockv1alpha2.DiscoverSpec{Seed: statedLayout()}
}

// initialRun is a run with a name, which is all draftFor reads of it.
func initialRun() *simplyblockv1alpha2.OperatorOps {
	return &simplyblockv1alpha2.OperatorOps{ObjectMeta: metav1.ObjectMeta{Name: "initial-discovery"}}
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

	draft, notes, err := r.draftFor(initialRun(), seeded(), discoverypkg.Plan{})
	if err != nil {
		t.Fatalf("the fleet was refused: %v", err)
	}

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

	draft, _, err := r.draftFor(initialRun(), seeded(), discoverypkg.Plan{})
	if err != nil {
		t.Fatalf("the fleet was refused: %v", err)
	}

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

// A run that states no seed gets discovery's own proposal.
func TestARunWithNoSeedGetsNone(t *testing.T) {
	r := &OperatorOpsReconciler{}
	theirs := &simplyblockv1alpha2.OperatorOps{
		ObjectMeta: metav1.ObjectMeta{Name: "discover-again"},
	}

	draft, _, err := r.draftFor(theirs, &simplyblockv1alpha2.DiscoverSpec{},
		discoverypkg.Plan{})
	if err != nil {
		t.Fatalf("the fleet was refused: %v", err)
	}

	cluster := draft.Spec.Cluster
	if cluster == nil {
		t.Fatal("the draft proposes no cluster")
	}
	if cluster.Name == "fleet-cluster" {
		t.Error("a run that stated no seed was given a cluster name")
	}
	if ptr.IntFrom(cluster.MaxSubsystemCount, 0) != int(discoverypkg.DefaultMaxSubsystemCount) {
		t.Errorf("maxSubsystemCount = %v, want discovery's own %d",
			cluster.MaxSubsystemCount, discoverypkg.DefaultMaxSubsystemCount)
	}
	if !ptr.BoolFromOrFalse(cluster.EnableDriveFormat) {
		t.Error("a run that stated no seed was given a refusal to format drives")
	}
	if cluster.EnableChecksumValidation != nil {
		t.Errorf("enableChecksumValidation = %v, want the cluster's own default to decide",
			cluster.EnableChecksumValidation)
	}
	if draft.Spec.EdgeCluster != nil {
		t.Error("a run that stated no seed has an edge draft")
	}
	if draft.Spec.Images != nil {
		t.Errorf("images = %+v, want a run that stated no seed to pin none", draft.Spec.Images)
	}
}

// A field the installation left alone stays discovery's, which is the whole point
// of the seed being partial: an installation states what it decided and nothing
// more.
func TestAnUnstatedFieldStaysDerived(t *testing.T) {
	r := &OperatorOpsReconciler{}
	partial := &simplyblockv1alpha2.DiscoverSpec{Seed: &simplyblockv1alpha2.DraftSeed{
		Cluster: &simplyblockv1alpha2.DraftSeedCluster{EnableChecksumValidation: ptr.To(true)},
	}}

	draft, _, err := r.draftFor(initialRun(), partial, discoverypkg.Plan{})
	if err != nil {
		t.Fatalf("the fleet was refused: %v", err)
	}

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
	spec := seeded()
	spec.ClusterRef = "simplyblock-cluster"

	draft, _, err := r.draftFor(initialRun(), spec, discoverypkg.Plan{})
	if err != nil {
		t.Fatalf("the fleet was refused: %v", err)
	}

	if draft.Spec.Cluster != nil {
		t.Errorf("a growth document proposes the layout %+v beside its clusterRef",
			draft.Spec.Cluster)
	}
	if draft.Spec.ClusterRef != "simplyblock-cluster" {
		t.Errorf("clusterRef = %q, want the cluster the run names", draft.Spec.ClusterRef)
	}
}
