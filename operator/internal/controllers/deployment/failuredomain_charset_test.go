// The character set a failure-domain label may use, checked against a real
// apiserver on the three fields that hold one: a deployment group's, a node's,
// and the cluster's index mapping. The three must agree, or an expansion can
// write a node the apiserver refuses.

package deployment

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// failureDomainLabels is each label and whether the schema admits it.
//
// A label that is only digits is admitted: a domain converted from v1alpha1, or
// recorded before the mapping existed, is its index spelled out, and the field is
// immutable once set, so refusing one would leave the node unfixable.
var failureDomainLabels = []struct {
	label  string
	admits bool
}{
	{"rack-b", true},
	{"rack_b", true},
	{"eu-central-1a", true},
	{"0", true},
	{"12", true},
	{"r", true},
	{"Rack-B", false},
	{"RACK", false},
	{"rack.b", false},
	{"-rack", false},
	{"rack-", false},
	{"_rack", false},
	{"rack b", false},
}

func TestAFailureDomainLabelIsLowercaseLettersDigitsHyphensAndUnderscores(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "fd-charset", Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(10)),
			VCPUCount:         ptr.To(int32(6)),
		},
	}
	if err := apiClient.Create(ctx, cluster); err != nil {
		t.Fatalf("storing the cluster: %v", err)
	}
	t.Cleanup(func() { _ = apiClient.Delete(ctx, cluster) })

	for _, tc := range failureDomainLabels {
		t.Run(tc.label, func(t *testing.T) {
			judge := func(field string, err error) {
				t.Helper()
				if tc.admits && err != nil {
					t.Errorf("%s %q was refused: %v", field, tc.label, err)
				}
				if !tc.admits && err == nil {
					t.Errorf("%s %q was admitted", field, tc.label)
				}
			}

			config := &simplyblockv1alpha2.ClusterDeploymentConfig{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "fd-", Namespace: "default"},
				Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
					Cluster: &simplyblockv1alpha2.ClusterTemplate{
						Name:              "fd-cluster",
						VCPUCount:         ptr.To(int32(4)),
						MaxSubsystemCount: ptr.To(int32(30)),
					},
					NodeSets: []simplyblockv1alpha2.NodeSet{{
						Name: "discovered",
						Groups: []simplyblockv1alpha2.NodeGroup{{
							Name:          "group-1",
							Workers:       []string{"worker-1"},
							FailureDomain: tc.label,
							Devices: &simplyblockv1alpha2.DeviceSelection{
								NVMe: []string{"0000:00:02.0"},
							},
						}},
					}},
				},
			}
			judge("ClusterDeploymentConfig group failureDomain",
				apiClient.Create(ctx, config, client.DryRunAll))

			node := &simplyblockv1alpha2.StorageNode{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "fd-", Namespace: "default"},
				Spec: simplyblockv1alpha2.StorageNodeSpec{
					ClusterRef: cluster.Name,
					WorkerNode: "worker-1",
					Slot:       ptr.To(int32(0)),
					Config: simplyblockv1alpha2.StorageNodeConfig{
						Sizing: simplyblockv1alpha2.StorageNodeSizing{
							VCPUCount: ptr.To(int32(4)),
						},
						FailureDomain: tc.label,
					},
				},
			}
			judge("StorageNode spec.config.failureDomain",
				apiClient.Create(ctx, node, client.DryRunAll))

			var fresh simplyblockv1alpha2.StorageCluster
			if err := apiClient.Get(ctx, client.ObjectKeyFromObject(cluster), &fresh); err != nil {
				t.Fatalf("reading the cluster: %v", err)
			}
			fresh.Status.FailureDomains = []simplyblockv1alpha2.FailureDomainIndex{
				{Name: tc.label, Index: 0},
			}
			judge("StorageCluster status.failureDomains name",
				apiClient.Status().Update(ctx, &fresh))
		})
	}
}
