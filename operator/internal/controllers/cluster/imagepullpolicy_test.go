// What the apiserver stamps on a StorageCluster that names no image pull
// policy, run against a real apiserver for the same reason the CEL cases are:
// a schema default is the apiserver's work, and asserting the marker instead of
// the stamped object asserts the source rather than the behavior.

package cluster

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The storage-node images this product ships are moving tags (main, main-latest,
// and the operator's own), so a node that comes back after a release has to pull
// rather than keep what its kubelet happens to hold. The workload builder has
// always meant Always: storage_node_workload.go falls back to it for an unset
// policy. The schema default is what that fallback never sees, because the
// apiserver stamps the field before any controller reads it.
func TestAStorageClusterThatNamesNoPullPolicyPullsAlways(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "pullpolicy-", Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(10)),
			VCPUCount:         ptr.To(int32(6)),
			// The block is present and the policy is not. A cluster with no
			// block at all is defaulted by nothing, since the apiserver fills
			// fields of an object that is there, and the workload builder's own
			// fallback covers that case.
			StorageNodes: &simplyblockv1alpha2.StorageNodesSpec{},
		},
	}
	if err := apiClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating the cluster: %v", err)
	}
	t.Cleanup(func() { _ = apiClient.Delete(ctx, cluster) })

	if got := cluster.Spec.StorageNodes.ImagePullPolicy; got != corev1.PullAlways {
		t.Errorf("a cluster that names no pull policy came back with %q, want %q",
			got, corev1.PullAlways)
	}
}
