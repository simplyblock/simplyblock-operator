// The storage-node agent's tolerations on a cluster with two-node host fencing:
// it must survive the out-of-service taint the storage fence remediation puts
// on a fenced node, because the control plane heals the node through it.

package utils

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func agentTolerations(cluster *simplyblockv1alpha2.StorageCluster) []corev1.Toleration {
	return BuildStorageNodeDaemonSet(cluster, false, false, "", "", "").Spec.Template.Spec.Tolerations
}

func dualCluster(hostFencing bool, tolerations ...corev1.Toleration) *simplyblockv1alpha2.StorageCluster {
	c := &simplyblockv1alpha2.StorageCluster{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "ns"}}
	c.Spec.ClusterMode = simplyblockv1alpha2.ClusterModeDual
	c.Spec.EnableArbitration = ptr.To(true)
	c.Spec.Arbitration = &simplyblockv1alpha2.ArbitrationSpec{HostFencing: ptr.To(hostFencing)}
	if len(tolerations) > 0 {
		c.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{Tolerations: tolerations}
	}
	return c
}

func tolerates(ts []corev1.Toleration) bool {
	return slices.ContainsFunc(ts, func(t corev1.Toleration) bool {
		return t.Key == simplyblockv1alpha2.TaintOutOfService && t.Effect == corev1.TaintEffectNoExecute
	})
}

func TestStorageNodeAgentToleratesOutOfServiceWithHostFencing(t *testing.T) {
	if !tolerates(agentTolerations(dualCluster(true))) {
		t.Fatal("with host fencing the agent must tolerate the out-of-service taint")
	}
}

// Every other cluster renders the DaemonSet exactly as before.
func TestStorageNodeAgentTolerationsUnchangedWithoutHostFencing(t *testing.T) {
	custom := corev1.Toleration{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "storage",
		Effect: corev1.TaintEffectNoSchedule}
	for name, c := range map[string]*simplyblockv1alpha2.StorageCluster{
		"dual without host fencing": dualCluster(false, custom),
		"standard":                  {ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"}},
	} {
		got := agentTolerations(c)
		if tolerates(got) {
			t.Errorf("%s: unexpected out-of-service toleration %v", name, got)
		}
	}
	if got := agentTolerations(dualCluster(false, custom)); !slices.Equal(got, []corev1.Toleration{custom}) {
		t.Errorf("configured tolerations changed: %v", got)
	}
}

// A configured toleration that already covers it is not doubled, and the
// configured list itself is never modified.
func TestStorageNodeAgentKeepsConfiguredTolerations(t *testing.T) {
	all := corev1.Toleration{Operator: corev1.TolerationOpExists}
	c := dualCluster(true, all)
	if got := agentTolerations(c); !slices.Equal(got, []corev1.Toleration{all}) {
		t.Errorf("tolerations %v", got)
	}
	custom := corev1.Toleration{Key: "dedicated", Operator: corev1.TolerationOpExists}
	c = dualCluster(true, custom)
	got := agentTolerations(c)
	if len(got) != 2 || !tolerates(got) || len(c.Spec.StorageNodes.Tolerations) != 1 {
		t.Errorf("tolerations %v, configured %v", got, c.Spec.StorageNodes.Tolerations)
	}
}
