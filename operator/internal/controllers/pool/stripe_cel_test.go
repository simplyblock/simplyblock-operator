// Validation of the striping rules compiled into the StoragePool CRD schema, run
// against a real apiserver, the way cel_validation_test.go runs the DHCHAP rule.
// They repeat what the CSI driver refuses at CreateVolume, so a pool that could
// never provision a striped volume is refused when it is written.

package pool

import (
	"context"
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

func TestStripeRulesInTheSchema(t *testing.T) {
	if err := simplyblockv1alpha2.AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: envtestAssets(),
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

	width := func(n int32) *int32 { return ptr.To(n) }
	for _, tc := range []struct {
		name     string
		defaults simplyblockv1alpha2.VolumeDefaults
		denial   string
	}{
		{name: "striped", defaults: simplyblockv1alpha2.VolumeDefaults{StripeWidth: width(4)}},
		{name: "striped with a chunk", defaults: simplyblockv1alpha2.VolumeDefaults{
			StripeWidth: width(2), StripeSize: "128Ki"}},
		{name: "a width of 1 with VDO is no stripe", defaults: simplyblockv1alpha2.VolumeDefaults{
			StripeWidth: width(1), EnableClientCompression: ptr.To(true)}},
		{name: "striped with replication off", defaults: simplyblockv1alpha2.VolumeDefaults{
			StripeWidth: width(2), EnableReplication: ptr.To(false)}},
		{name: "LVM's ceiling", defaults: simplyblockv1alpha2.VolumeDefaults{StripeWidth: width(128)}},

		{name: "above LVM's ceiling", defaults: simplyblockv1alpha2.VolumeDefaults{StripeWidth: width(129)},
			denial: "stripeWidth"},
		{name: "a chunk that is not a power of two", defaults: simplyblockv1alpha2.VolumeDefaults{
			StripeWidth: width(2), StripeSize: "96Ki"}, denial: "stripeSize"},
		{name: "a chunk without a stripe", defaults: simplyblockv1alpha2.VolumeDefaults{StripeSize: "64Ki"},
			denial: "stripeSize requires a stripeWidth of 2 or more"},
		{name: "striped with client-side compression", defaults: simplyblockv1alpha2.VolumeDefaults{
			StripeWidth: width(2), EnableClientCompression: ptr.To(true)},
			denial: "cannot use client-side compression or deduplication"},
		{name: "striped with client-side deduplication", defaults: simplyblockv1alpha2.VolumeDefaults{
			StripeWidth: width(3), EnableClientDeduplication: ptr.To(true)},
			denial: "cannot use client-side compression or deduplication"},
		{name: "striped with replication", defaults: simplyblockv1alpha2.VolumeDefaults{
			StripeWidth: width(2), EnableReplication: ptr.To(true)}, denial: "cannot be replicated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defaults := tc.defaults
			pool := &simplyblockv1alpha2.StoragePool{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "stripe-", Namespace: "default"},
				Spec:       simplyblockv1alpha2.StoragePoolSpec{ClusterRef: testCluster, VolumeDefaults: &defaults},
			}
			err := apiClient.Create(context.Background(), pool)
			switch {
			case tc.denial != "" && err == nil:
				t.Fatal("expected the apiserver to reject the pool, but it was accepted")
			case tc.denial != "" && !strings.Contains(err.Error(), tc.denial):
				t.Fatalf("rejected for the wrong reason: %v", err)
			case tc.denial == "" && err != nil:
				t.Fatalf("expected the apiserver to accept the pool, got: %v", err)
			}
		})
	}
}
