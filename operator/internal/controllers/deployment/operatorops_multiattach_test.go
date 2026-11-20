// A discovery run told about the storage array drafts a multi-attach document:
// the array goes into the draft, and what the run found about the workers'
// initiators reaches the run's findings.

package deployment

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	discoverypkg "github.com/simplyblock/simplyblock-operator/internal/discovery"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

func TestADiscoveryRunToldTheArrayDraftsAMultiAttachDocument(t *testing.T) {
	r := &OperatorOpsReconciler{Client: fake.NewClientBuilder().WithScheme(opsScheme(t)).Build()}
	ops := &simplyblockv1alpha2.OperatorOps{ObjectMeta: metav1.ObjectMeta{Name: "discover-1"}}
	spec := &simplyblockv1alpha2.DiscoverSpec{
		EnableLogicalBlockDevices: boolPointer(true),
		MultiAttach: &simplyblockv1alpha2.MultiAttachSpec{
			FaultTolerance: 1,
			Array: &simplyblockv1alpha2.StorageArraySpec{
				Plugin: "pure-flasharray", Endpoint: "https://pure01.example",
				Protocol: simplyblockv1alpha2.StorageArrayProtocolISCSI,
				LunSize:  resource.MustParse("4Ti"),
				Credentials: simplyblockv1alpha2.StorageArrayCredentials{
					Source:    simplyblockv1alpha2.StorageArrayCredentialsSourceSecret,
					SecretRef: &corev1.LocalObjectReference{Name: "pure01"},
				},
			},
		},
	}
	plan := discoverypkg.Plan{Class: discoverypkg.ClassBlock, Workers: []discoverypkg.Worker{
		{Name: "worker-01", Class: discoverypkg.ClassBlock,
			Report: nodeprobe.Report{Node: "worker-01", SANInitiators: &nodeprobe.SANInitiators{ISCSIIQN: "iqn.a"}}},
		{Name: "worker-02", Class: discoverypkg.ClassBlock, Report: nodeprobe.Report{Node: "worker-02"}},
	}}

	config, _, err := r.draftFor(context.Background(), ops, spec, &plan)
	if err != nil {
		t.Fatalf("the fleet was refused: %v", err)
	}
	ma := config.Spec.Cluster.MultiAttach
	if ma == nil || ma.Array == nil || ma.Array.Plugin != "pure-flasharray" {
		t.Fatalf("the draft carries multiAttach %+v", ma)
	}
	if config.Spec.Cluster.RaidDevices == nil || !*config.Spec.Cluster.RaidDevices {
		t.Error("the draft does not declare raidDevices")
	}
	if len(plan.Findings) != 1 || plan.Findings[0].Reason != discoverypkg.ArrayInitiatorMissing {
		t.Errorf("the run's findings are %+v, want worker-02's missing initiator", plan.Findings)
	}
}

func boolPointer(b bool) *bool { return &b }
