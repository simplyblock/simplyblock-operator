// What this package's envtest-backed tests need to find a real apiserver, and
// nothing else.
//
// One test here cannot run against a fake client: the CRD's CEL rules are
// evaluated by the apiserver, and this kind leans on them harder than most.
// The agreement between spec.action and its parameter block, and the
// immutability of everything but spec.abort, are declared on the type rather
// than checked in the webhook, because both are statements about one object's
// own fields (design-persistentvolumeops.md §4.3).

package volume

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
// CRDs installed, starting one on first use.
func apiServer(t *testing.T) client.Client {
	t.Helper()
	sharedEnvOnce.Do(func() {
		if err := simplyblockv1alpha2.AddToScheme(scheme.Scheme); err != nil {
			sharedEnvErr = err
			return
		}
		sharedEnv = &envtest.Environment{
			CRDDirectoryPaths: []string{
				filepath.Join("..", "..", "..", "config", "crd", "bases"),
			},
			ErrorIfCRDPathMissing: true,
			BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
		}
		cfg, err := sharedEnv.Start()
		if err != nil {
			sharedEnvErr = err
			return
		}
		sharedEnvClient, sharedEnvErr = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	})
	if sharedEnvErr != nil {
		t.Fatalf("starting the test apiserver: %v", sharedEnvErr)
	}
	return sharedEnvClient
}

// getFirstFoundEnvTestBinaryDir locates the envtest asset binaries.
//
// controller-runtime normally passes them through KUBEBUILDER_ASSETS, which the
// Makefile sets. This is what makes the same test runnable straight from an
// editor, and it reads the shared repository-root .bin that
// `make setup-envtest` populates.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "..", "..", ".bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "the envtest assets could not be read", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
