// What this package's one envtest-backed test needs to find a real apiserver.
//
// The rest of the package is table-driven over the workload builders and needs
// no cluster, but a CEL rule compiled into the CRD schema is evaluated by the
// apiserver and by nothing else, so the only way to prove it rejects what it
// should is to ask a real one. The arrangement is the cluster package's,
// including the single shared server: envtest costs several seconds to start.

package driver

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

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedEnv != nil {
		_ = sharedEnv.Stop()
	}
	os.Exit(code)
}

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
			BinaryAssetsDirectory: envTestBinaryDir(),
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

// envTestBinaryDir locates the envtest assets the repository-root .bin holds,
// so the test runs from an editor as well as from the Makefile, which sets
// KUBEBUILDER_ASSETS itself.
func envTestBinaryDir() string {
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
