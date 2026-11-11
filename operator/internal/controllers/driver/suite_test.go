// The apiserver the CRD validation tests of this package run against. The
// driver's other tests use a fake client, which neither defaults a field nor
// evaluates a schema bound, and both are the apiserver's.
//
// One apiserver serves every test that asks, started on first use and stopped
// when the package's tests finish.

package driver

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

var (
	sharedEnvOnce   sync.Once
	sharedEnv       *envtest.Environment
	sharedEnvClient client.Client
	sharedEnvErr    error
)

// TestMain stops the shared apiserver once, after every test that used it.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedEnv != nil {
		_ = sharedEnv.Stop()
	}
	os.Exit(code)
}

// apiServer returns a client against a real apiserver with this repository's
// CRDs installed. Only v1alpha2 is exercised: the CRDs carry a conversion
// webhook envtest does not run.
func apiServer(t *testing.T) client.Client {
	t.Helper()
	sharedEnvOnce.Do(func() {
		if err := simplyblockv1alpha2.AddToScheme(scheme.Scheme); err != nil {
			sharedEnvErr = err
			return
		}
		sharedEnv = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
			BinaryAssetsDirectory: envtestAssets(),
		}
		cfg, err := sharedEnv.Start()
		if err != nil {
			sharedEnvErr = err
			return
		}
		sharedEnvClient, sharedEnvErr = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	})
	if sharedEnvErr != nil {
		t.Skipf("no test apiserver (set KUBEBUILDER_ASSETS or run make setup-envtest): %v", sharedEnvErr)
	}
	return sharedEnvClient
}

// envtestAssets is the first asset directory `make setup-envtest` populated
// under the repository's .bin, or empty so KUBEBUILDER_ASSETS decides.
func envtestAssets() string {
	base := filepath.Join("..", "..", "..", "..", ".bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}
