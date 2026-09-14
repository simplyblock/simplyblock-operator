// What the pool a StorageCluster is created with actually looks like once the
// apiserver has applied the CRD's defaults to it.
//
// This needs a real apiserver and cannot be asserted against a fake client. The
// filesystem a pool's volumes are formatted with is declared as a default on
// spec.volumeDefaults.filesystem, and structural defaulting only reaches inside
// an object that is present: a pool written with spec.volumeDefaults absent
// comes back with no filesystem, its generated StorageClass carries no
// csi.storage.k8s.io/fstype, and the node plugin formats ext4 instead. Nothing
// in the operator says ext4 anywhere, so the only place that shows up is a
// mounted volume.
package controller

import (
	"context"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/pool"
)

// TestTheDefaultPoolIsFormattedXFS drives ensureDefaultPool against a real
// apiserver and reads back the pool it wrote. Going through the reconciler
// rather than restating its spec is the whole point: what is being asserted is
// that the pool the operator writes picks the defaults up, and a copy of the
// spec here would keep passing after the operator stopped writing it that way.
func TestTheDefaultPoolIsFormattedXFS(t *testing.T) {
	if err := simplyblockv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("adding the v1alpha1 scheme: %v", err)
	}
	if err := simplyblockv1alpha2.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("adding the v1alpha2 scheme: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})

	apiClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("building a client: %v", err)
	}

	ctx := context.Background()
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(10)),
			VCPUCount:         ptr.To(int32(6)),
		},
	}
	if err := apiClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating the cluster: %v", err)
	}

	r := &StorageClusterReconciler{Client: apiClient, Scheme: scheme.Scheme}
	r.ensureDefaultPool(ctx, cluster)

	var stored simplyblockv1alpha2.StoragePool
	key := client.ObjectKey{Namespace: "default", Name: pool.DefaultPoolName(cluster.Name)}
	if err := apiClient.Get(ctx, key, &stored); err != nil {
		t.Fatalf("the cluster's default pool was not written: %v", err)
	}

	if stored.Spec.VolumeDefaults == nil {
		t.Fatal("the default pool has no volume defaults, so its class carries no filesystem " +
			"and its volumes are formatted ext4")
	}
	if got := stored.Spec.VolumeDefaults.Filesystem; got != "xfs" {
		t.Errorf("the default pool's filesystem is %q, want xfs", got)
	}
	if d := stored.Spec.VolumeDefaults; d.IOPS != nil || d.Throughput != nil ||
		d.MaxNamespacesPerSubsystem != nil {
		t.Errorf("the default pool states a ceiling nobody asked for: %+v", d)
	}
	if stored.Spec.Limits != nil {
		t.Errorf("the default pool states pool limits nobody asked for: %+v", stored.Spec.Limits)
	}
}
