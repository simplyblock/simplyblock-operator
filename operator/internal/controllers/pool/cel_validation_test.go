// Validation of the CEL rule compiled into the StoragePool CRD schema, run
// against a real apiserver. No webhook is involved: the apiserver enforces the
// rule itself, and envtest is the only place in the tree that starts one.

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

// Regression: 2026-09-29-pool-allowednodes-without-dhchap — a pool that named
// allowedNodes without volumeDefaults.enableDHCHAP was admitted and reported
// Ready, but the control plane refuses to register hosts on a pool without
// DHCHAP, so every reconcile failed and the list restricted nothing.
//
// I-20: every combination of the two fields; only nodes without DHCHAP is denied.
func TestAllowedNodesRequireDHCHAP(t *testing.T) {
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

	nodes := []string{"worker-1", "worker-2"}
	dhchap := func(on bool) *simplyblockv1alpha2.VolumeDefaults {
		return &simplyblockv1alpha2.VolumeDefaults{EnableDHCHAP: ptr.To(on)}
	}

	for _, tc := range []struct {
		name       string
		nodes      []string
		defaults   *simplyblockv1alpha2.VolumeDefaults
		wantDenied bool
	}{
		{name: "neither set"},
		{name: "DHCHAP without nodes", defaults: dhchap(true)},
		{name: "nodes with DHCHAP", nodes: nodes, defaults: dhchap(true)},
		{name: "nodes alone", nodes: nodes, wantDenied: true},
		{name: "nodes with DHCHAP false", nodes: nodes, defaults: dhchap(false), wantDenied: true},
		{name: "nodes with defaults that omit DHCHAP", nodes: nodes,
			defaults: &simplyblockv1alpha2.VolumeDefaults{Filesystem: "xfs"}, wantDenied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := &simplyblockv1alpha2.StoragePool{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-", Namespace: "default"},
				Spec: simplyblockv1alpha2.StoragePoolSpec{
					ClusterRef: testCluster, AllowedNodes: tc.nodes, VolumeDefaults: tc.defaults,
				},
			}
			err := apiClient.Create(context.Background(), pool)
			switch {
			case tc.wantDenied && err == nil:
				t.Fatal("expected the apiserver to reject the pool, but it was accepted")
			case tc.wantDenied && !strings.Contains(err.Error(), "allowedNodes requires volumeDefaults.enableDHCHAP"):
				t.Fatalf("rejected for the wrong reason: %v", err)
			case !tc.wantDenied && err != nil:
				t.Fatalf("expected the apiserver to accept the pool, got: %v", err)
			}
		})
	}
}

// envtestAssets locates the binaries `make setup-envtest` leaves in the
// repository-root .bin, so the test runs straight from an editor.
func envtestAssets() string {
	base := filepath.Join("..", "..", "..", "..", ".bin", "k8s")
	entries, _ := os.ReadDir(base)
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}
	return ""
}
