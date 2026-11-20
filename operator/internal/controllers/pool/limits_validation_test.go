// Validation of the CEL rule on StoragePool spec.limits, run against a real
// apiserver. The rule refuses the pool-wide IOPS and throughput ceilings and leaves
// capacity and maxVolumeSize alone.

package pool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Regression: 2026-09-30-pool-qos-limits-corrupt-spdk — pool-wide IOPS and throughput
// limits were admitted, but SPDK frees the pool's limits array while volumes still
// use it: the IOPS limit was not enforced, the throughput limit applied per volume,
// and SPDK crashed on all three storage nodes.
//
// I-21: only a pool that sets limits.iops or limits.throughput is denied.
func TestPoolLimitsRejectIOPSAndThroughput(t *testing.T) {
	if err := simplyblockv1alpha2.AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: limitsEnvtestAssets(),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting the test apiserver: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	apiClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		limits     *simplyblockv1alpha2.PoolLimits
		wantDenied bool
	}{
		{name: "no limits"},
		{name: "capacity and maxVolumeSize",
			limits: &simplyblockv1alpha2.PoolLimits{Capacity: "10G", MaxVolumeSize: "2G"}},
		{name: "iops", limits: &simplyblockv1alpha2.PoolLimits{IOPS: ptr.To(int32(1000))}, wantDenied: true},
		{name: "iops zero", limits: &simplyblockv1alpha2.PoolLimits{IOPS: ptr.To(int32(0))}, wantDenied: true},
		{name: "throughput readWrite", wantDenied: true,
			limits: &simplyblockv1alpha2.PoolLimits{
				Throughput: &simplyblockv1alpha2.ThroughputLimits{ReadWrite: ptr.To(int32(20))}}},
		{name: "empty throughput", wantDenied: true,
			limits: &simplyblockv1alpha2.PoolLimits{Throughput: &simplyblockv1alpha2.ThroughputLimits{}}},
		{name: "capacity with iops", wantDenied: true,
			limits: &simplyblockv1alpha2.PoolLimits{Capacity: "10G", IOPS: ptr.To(int32(1000))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := &simplyblockv1alpha2.StoragePool{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "limits-", Namespace: "default"},
				Spec:       simplyblockv1alpha2.StoragePoolSpec{ClusterRef: testCluster, Limits: tc.limits},
			}
			err := apiClient.Create(context.Background(), pool)
			switch {
			case tc.wantDenied && err == nil:
				t.Fatal("expected the apiserver to reject the pool, but it was accepted")
			case tc.wantDenied && !strings.Contains(err.Error(), "limits.iops and limits.throughput are not yet supported in beta1"):
				t.Fatalf("rejected for the wrong reason: %v", err)
			case !tc.wantDenied && err != nil:
				t.Fatalf("expected the apiserver to accept the pool, got: %v", err)
			}
		})
	}
}

// limitsEnvtestAssets locates the binaries `make setup-envtest` leaves in the
// repository-root .bin, so the test runs straight from an editor.
func limitsEnvtestAssets() string {
	base := filepath.Join("..", "..", "..", "..", ".bin", "k8s")
	entries, _ := os.ReadDir(base)
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}
	return ""
}
