package cluster

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// TestStorageClusterCELValidatesArbitration covers spec.arbitration's schema:
// holdMs must stay below the hosts' keep-alive (≤ 4500 ms), a lease must be
// shorter than the hold it guards, and arbitration is only valid on a Dual
// cluster.
func TestStorageClusterCELValidatesArbitration(t *testing.T) {
	apiClient := apiServer(t)
	const onlyDual = "only valid when clusterMode is Dual"

	for _, tc := range []struct {
		name        string
		mode        simplyblockv1alpha2.ClusterMode
		enable      *bool
		arbitration *simplyblockv1alpha2.ArbitrationSpec
		wantDenied  string
	}{
		{name: "unset"},
		{name: "dual without arbitration", mode: simplyblockv1alpha2.ClusterModeDual},
		{name: "arbitration with defaults", mode: simplyblockv1alpha2.ClusterModeDual, enable: ptr.To(true)},
		{name: "lease shorter than hold", mode: simplyblockv1alpha2.ClusterModeDual, enable: ptr.To(true),
			arbitration: &simplyblockv1alpha2.ArbitrationSpec{HoldMs: ptr.To(int32(2500)), LeaseTtlMs: ptr.To(int32(1500))}},
		{name: "lease not shorter than hold", mode: simplyblockv1alpha2.ClusterModeDual, enable: ptr.To(true),
			arbitration: &simplyblockv1alpha2.ArbitrationSpec{HoldMs: ptr.To(int32(1500)), LeaseTtlMs: ptr.To(int32(1500))},
			wantDenied:  "leaseTtlMs must be shorter than holdMs"},
		{name: "hold above the keep-alive", mode: simplyblockv1alpha2.ClusterModeDual, enable: ptr.To(true),
			arbitration: &simplyblockv1alpha2.ArbitrationSpec{HoldMs: ptr.To(int32(5000))}, wantDenied: "holdMs"},
		{name: "hold below the floor", mode: simplyblockv1alpha2.ClusterModeDual, enable: ptr.To(true),
			arbitration: &simplyblockv1alpha2.ArbitrationSpec{HoldMs: ptr.To(int32(100))}, wantDenied: "holdMs"},
		{name: "arbitration on a standard cluster", mode: simplyblockv1alpha2.ClusterModeStandard,
			enable: ptr.To(true), wantDenied: onlyDual},
		{name: "arbitration on a cluster of unstated mode", enable: ptr.To(true), wantDenied: onlyDual},
		{name: "arbitration on a single cluster", mode: simplyblockv1alpha2.ClusterModeSingle,
			enable: ptr.To(true), wantDenied: onlyDual},
		{name: "arbitration settings on a standard cluster", mode: simplyblockv1alpha2.ClusterModeStandard,
			arbitration: &simplyblockv1alpha2.ArbitrationSpec{HoldMs: ptr.To(int32(2500))}, wantDenied: onlyDual},
		{name: "arbitration off on a standard cluster", mode: simplyblockv1alpha2.ClusterModeStandard,
			enable: ptr.To(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &simplyblockv1alpha2.StorageCluster{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-2n-", Namespace: "default"},
				Spec: simplyblockv1alpha2.StorageClusterSpec{
					MaxSubsystemCount: ptr.To(int32(10)),
					VCPUCount:         ptr.To(int32(6)),
					ClusterMode:       tc.mode,
					EnableArbitration: tc.enable,
					Arbitration:       tc.arbitration,
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
