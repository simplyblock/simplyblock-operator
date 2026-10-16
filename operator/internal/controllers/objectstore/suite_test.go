// What this package's envtest-backed tests need to find a real apiserver, and
// nothing else.
//
// S3Bucket's reconciler (W-03, design-bucket-provisioning.md) drives a PVC to
// Bound, finds the NFSExport bound to it, and delivers the bucket's mount to a
// gateway's node set over the in-process csi-link registry: three things a
// fake client cannot stand in for. PVC/PV binding and NFSExport readiness are
// the CSI controller's and NFSExportReconciler's own behavior, neither of
// which runs here, and the registry is a real (if faked-at-the-boundary) gRPC
// link. internal/controllers/objectstore's own fakes play those three parts
// (fake_csi_binder_test.go, fake_nfsexport_test.go, fake_peerregistry_test.go)
// so the reconciler's own state machine and RBAC can still be proven against a
// real apiserver, per the design's Testing Strategy (§11).
//
// One apiserver serves every test in this package. It is started on the first
// test that asks for one and stopped when the package's tests finish, rather
// than started per test: envtest costs several seconds to come up, and this
// package's tests are most of its own runtime for no extra coverage if it paid
// that cost repeatedly.

package objectstore

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
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
// CRDs installed, starting one on first use. PersistentVolumeClaim and
// PersistentVolume come from client-go's own scheme. NFSExport and S3Bucket
// need this repository's two API groups added to it as well.
func apiServer(t *testing.T) client.Client {
	t.Helper()
	sharedEnvOnce.Do(func() {
		if err := simplyblockv1alpha2.AddToScheme(scheme.Scheme); err != nil {
			sharedEnvErr = err
			return
		}
		if err := objectstorev1alpha1.AddToScheme(scheme.Scheme); err != nil {
			sharedEnvErr = err
			return
		}
		sharedEnv = &envtest.Environment{
			CRDDirectoryPaths: []string{
				filepath.Join("..", "..", "..", "config", "crd", "bases"),
				// Vendored from cert-manager v1.21.2's own release
				// (cert-manager.crds.yaml), the only consumer of the
				// cert-manager-provider TLS path today
				// (objectstore_controller.go, design-objectstore.md §5).
				// cert-manager's own controller does not run here, so a
				// Certificate this package creates never reaches Ready on
				// its own; a test that needs one Ready sets status directly,
				// same as this suite already does for Deployment.status.
				filepath.Join("testdata"),
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
