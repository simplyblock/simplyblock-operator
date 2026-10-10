package cluster

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// TestStorageClusterCELValidatesTwoNode covers spec.twoNode's schema: holdMs
// must stay below the hosts' keep-alive (≤ 4500 ms), and a lease must be
// shorter than the hold it guards.
func TestStorageClusterCELValidatesTwoNode(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name       string
		twoNode    *simplyblockv1alpha2.TwoNodeSpec
		wantDenied string
	}{
		{name: "unset"},
		{name: "arbitration with defaults", twoNode: &simplyblockv1alpha2.TwoNodeSpec{Arbitration: true, PreferredNode: "worker-a"}},
		{name: "lease shorter than hold", twoNode: &simplyblockv1alpha2.TwoNodeSpec{Arbitration: true,
			HoldMs: ptr.To(int32(2500)), LeaseTtlMs: ptr.To(int32(1500))}},
		{name: "lease not shorter than hold", twoNode: &simplyblockv1alpha2.TwoNodeSpec{Arbitration: true,
			HoldMs: ptr.To(int32(1500)), LeaseTtlMs: ptr.To(int32(1500))}, wantDenied: "leaseTtlMs must be shorter than holdMs"},
		{name: "hold above the keep-alive", twoNode: &simplyblockv1alpha2.TwoNodeSpec{Arbitration: true,
			HoldMs: ptr.To(int32(5000))}, wantDenied: "holdMs"},
		{name: "hold below the floor", twoNode: &simplyblockv1alpha2.TwoNodeSpec{Arbitration: true,
			HoldMs: ptr.To(int32(100))}, wantDenied: "holdMs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &simplyblockv1alpha2.StorageCluster{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-2n-", Namespace: "default"},
				Spec: simplyblockv1alpha2.StorageClusterSpec{
					MaxSubsystemCount: ptr.To(int32(10)),
					VCPUCount:         ptr.To(int32(6)),
					TwoNode:           tc.twoNode,
				},
			}
			err := apiClient.Create(context.Background(), cluster)
			if tc.wantDenied != "" {
				if err == nil {
					t.Fatal("expected the apiserver to reject the cluster, but it was accepted")
				}
				if !strings.Contains(err.Error(), tc.wantDenied) {
					t.Fatalf("rejected for the wrong reason: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected the apiserver to accept the cluster, got: %v", err)
			}
			_ = apiClient.Delete(context.Background(), cluster)
		})
	}
}
