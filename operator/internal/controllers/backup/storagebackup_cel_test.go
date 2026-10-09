// The CEL rule that makes a StorageBackup either a record (clusterRef) or a
// request (source), run against a real apiserver because only one evaluates it.

package backup

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

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// envtestAssets is KUBEBUILDER_ASSETS, or the first version under the
// repository's .bin/k8s, which is what lets the test run from an editor.
func envtestAssets() string {
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	base := filepath.Join("..", "..", "..", "..", ".bin", "k8s")
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

func TestStorageBackupSpecIsEitherARecordOrARequest(t *testing.T) {
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
		t.Fatalf("start the test apiserver: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}

	request := &simplyblockv1alpha2.BackupRequest{ClaimName: testClaim}
	for name, tc := range map[string]struct {
		spec simplyblockv1alpha2.StorageBackupSpec
		want string // empty means admitted
	}{
		"a record":  {spec: simplyblockv1alpha2.StorageBackupSpec{ClusterRef: "production"}},
		"a request": {spec: simplyblockv1alpha2.StorageBackupSpec{Source: request}},
		"both": {
			spec: simplyblockv1alpha2.StorageBackupSpec{ClusterRef: "production", Source: request},
			want: "exactly one of source",
		},
		"neither": {want: "exactly one of source"},
	} {
		t.Run(name, func(t *testing.T) {
			obj := &simplyblockv1alpha2.StorageBackup{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-", Namespace: "default"}, Spec: tc.spec,
			}
			err := c.Create(context.Background(), obj)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("rejected: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("err = %v, want a rejection mentioning %q", err, tc.want)
			}
		})
	}
}
