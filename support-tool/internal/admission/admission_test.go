// The schema and CEL rules of the operator's CRDs, evaluated offline against
// documents admission refuses and documents it takes.

package admission

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func discoveryRun(spec *simplyblockv1alpha2.DiscoverSpec) *simplyblockv1alpha2.OperatorOps {
	return &simplyblockv1alpha2.OperatorOps{
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.simplyblock.io/v1alpha2", Kind: "OperatorOps"},
		ObjectMeta: metav1.ObjectMeta{Name: "discovery", Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha2.OperatorOpsSpec{Action: simplyblockv1alpha2.OperatorOpsActionDiscover, Discover: spec},
	}
}

func refusals(t *testing.T, obj interface{ GetName() string }) []string {
	t.Helper()
	found, err := Validate(obj.(runtimeObject))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return found
}

func contains(found []string, text string) bool {
	for _, f := range found {
		if strings.Contains(f, text) {
			return true
		}
	}
	return false
}

func TestARunNamingWorkersAndANodeSelectorIsRefused(t *testing.T) {
	found := refusals(t, discoveryRun(&simplyblockv1alpha2.DiscoverSpec{
		Workers:      []string{"worker-1"},
		NodeSelector: map[string]string{"role": "storage"},
	}))
	if !contains(found, "state one or the other") {
		t.Errorf("the refusals are %v", found)
	}
}

func TestAValidRunIsAdmitted(t *testing.T) {
	if found := refusals(t, discoveryRun(&simplyblockv1alpha2.DiscoverSpec{
		Workers: []string{"worker-1"}, EnableLogicalBlockDevices: ptr.To(true),
	})); len(found) != 0 {
		t.Errorf("a valid run is refused: %v", found)
	}
}

func cdc(env simplyblockv1alpha2.KubernetesEnvironment, edit func(*simplyblockv1alpha2.ClusterTemplate)) *simplyblockv1alpha2.ClusterDeploymentConfig {
	c := &simplyblockv1alpha2.ClusterDeploymentConfig{
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.simplyblock.io/v1alpha2", Kind: "ClusterDeploymentConfig"},
		ObjectMeta: metav1.ObjectMeta{Name: "draft", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
			Environment: env,
			Cluster: &simplyblockv1alpha2.ClusterTemplate{
				Name: "draft-cluster", VCPUCount: ptr.To(int32(4)), MaxSubsystemCount: ptr.To(int32(30)),
			},
			NodeSets: []simplyblockv1alpha2.NodeSet{{Name: "discovered", Groups: []simplyblockv1alpha2.NodeGroup{{
				Name: "group-1", Workers: []string{"worker-1"},
				Devices: &simplyblockv1alpha2.DeviceSelection{NVMe: []string{"0000:5e:00.0"}},
			}}}},
		},
	}
	if edit != nil {
		edit(c.Spec.Cluster)
	}
	return c
}

func TestATalosDocumentStatingTheVanillaFlagsIsRefused(t *testing.T) {
	found := refusals(t, cdc(simplyblockv1alpha2.KubernetesEnvironmentTalos, func(c *simplyblockv1alpha2.ClusterTemplate) {
		c.EnableCpuTopology = ptr.To(true)
	}))
	if !contains(found, "Vanilla") {
		t.Errorf("the refusals are %v", found)
	}
	if found := refusals(t, cdc(simplyblockv1alpha2.KubernetesEnvironmentVanilla, func(c *simplyblockv1alpha2.ClusterTemplate) {
		c.EnableCpuTopology = ptr.To(true)
	})); len(found) != 0 {
		t.Errorf("a Vanilla document stating the flag is refused: %v", found)
	}
}

func TestAnEnvironmentOutsideTheSetIsRefused(t *testing.T) {
	if found := refusals(t, cdc("Nomad", nil)); !contains(found, "environment") {
		t.Errorf("the refusals are %v", found)
	}
}

func TestADuplicateInASetIsRefused(t *testing.T) {
	found := refusals(t, discoveryRun(&simplyblockv1alpha2.DiscoverSpec{
		DeviceFilter: &simplyblockv1alpha2.DeviceFilter{PcieAllowList: []string{"0000:5e:00.0", "0000:5e:00.0"}},
	}))
	if !contains(found, "pcieAllowList") || !contains(found, "Duplicate") {
		t.Errorf("a set listing an entry twice is refused with %v", found)
	}
}
