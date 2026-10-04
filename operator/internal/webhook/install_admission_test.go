// What a fresh install's own objects must survive: admission, on a cluster where
// the operator serving the webhooks is part of the same install and is not up
// yet.
//
// The chart renders a SimplyblockDriver beside the Deployment that validates it
// (design-simplyblockdriver.md §8), so on a first install the API server is asked
// to admit that object seconds after the webhook configuration is registered and
// roughly a minute before the operator answers on the service. Whether that
// succeeds is decided by one field of the generated configuration, which is why
// this test reads the generated file rather than a copy of it.
//
// It lives beside the validators rather than under a chart test because the field
// it pins is written as a kubebuilder marker in this package, and `helm-sync`
// copies it into the chart from here.

package webhook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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

// TestMain stops the shared apiserver once, after every test that used one.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedEnv != nil {
		_ = sharedEnv.Stop()
	}
	os.Exit(code)
}

// apiServer returns a client against a real apiserver with this repository's
// CRDs installed, starting one on first use.
//
// A real one is the point here: admission is the apiserver's, and a fake client
// admits everything, so the behavior this file is about is invisible below this
// level.
func apiServer(t *testing.T) client.Client {
	t.Helper()
	sharedEnvOnce.Do(func() {
		if err := simplyblockv1alpha2.AddToScheme(scheme.Scheme); err != nil {
			sharedEnvErr = err
			return
		}
		sharedEnv = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
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

// envTestBinaryDir locates the envtest asset binaries in the repository-root
// .bin that `make setup-envtest` populates, so that the test runs from an editor
// as well as from the Makefile.
func envTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "..", ".bin", "k8s")
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

// registerGeneratedWebhooks applies the webhook configurations exactly as
// `make manifests` generates them, and points nothing at a running server.
//
// The generated clientConfig names a service in a namespace neither of which
// exists here, which is the same condition a first install is in: the
// configuration is registered, and the endpoint behind it is not there yet. What
// the apiserver does about that is the failurePolicy's answer, and that is the
// field under test.
func registerGeneratedWebhooks(t *testing.T, c client.Client) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "webhook", "manifests.yaml"))
	if err != nil {
		t.Fatalf("reading the generated webhook manifests: %v", err)
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decoding the generated webhook manifests: %v", err)
		}
		if len(object.Object) == 0 {
			continue
		}
		if err := c.Create(context.Background(), object); err != nil {
			t.Fatalf("registering %s/%s: %v", object.GetKind(), object.GetName(), err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), object) })
	}
}

// TestBootstrapDriverIsAdmittedWhileTheOperatorIsNotServing covers the object the
// chart writes on a first install.
//
// Regression: 2026-09-20-fresh-install-blocked-by-driver-webhook — a first
// `helm install` of the chart failed with `Internal error occurred: failed
// calling webhook "vsimplyblockdriver.simplyblock.io": no endpoints available for
// service "simplyblock-operator-webhook-service"`. The release stopped at the
// SimplyblockDriver, which left a cluster carrying an operator, a control plane,
// and no CSI driver, and the install had to be run a second time to complete.
func TestBootstrapDriverIsAdmittedWhileTheOperatorIsNotServing(t *testing.T) {
	c := apiServer(t)
	registerGeneratedWebhooks(t, c)

	driver := &simplyblockv1alpha2.SimplyblockDriver{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock", Namespace: "default"},
	}
	if err := c.Create(context.Background(), driver); err != nil {
		t.Fatalf("the chart's SimplyblockDriver was refused while the operator was not serving, "+
			"which is every first install of the chart: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), driver) })
}
