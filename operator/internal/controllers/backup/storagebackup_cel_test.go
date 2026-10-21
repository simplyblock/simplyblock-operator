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

// celClient starts a test apiserver with this repository's CRDs installed.
func celClient(t *testing.T) client.Client {
	t.Helper()
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
	return c
}

// admitted creates obj and checks the apiserver's answer: admitted when want is
// empty, otherwise rejected with a message containing want.
func admitted(t *testing.T, c client.Client, obj client.Object, want string) {
	t.Helper()
	err := c.Create(context.Background(), obj)
	switch {
	case want == "" && err != nil:
		t.Errorf("rejected: %v", err)
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		t.Errorf("err = %v, want a rejection mentioning %q", err, want)
	}
}

func TestStorageBackupSpecIsEitherARecordOrARequest(t *testing.T) {
	c := celClient(t)

	request := &simplyblockv1alpha2.BackupRequest{ClaimName: testClaim}
	group := &simplyblockv1alpha2.BackupRequest{ConsistencyGroupName: "orders"}
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
		"neither":         {want: "exactly one of source"},
		"a group request": {spec: simplyblockv1alpha2.StorageBackupSpec{Source: group}},
		"a request naming a claim and a group": {
			spec: simplyblockv1alpha2.StorageBackupSpec{Source: &simplyblockv1alpha2.BackupRequest{
				ClaimName: testClaim, ConsistencyGroupName: "orders",
			}},
			want: "exactly one of claimName",
		},
		"a request naming nothing": {
			spec: simplyblockv1alpha2.StorageBackupSpec{Source: &simplyblockv1alpha2.BackupRequest{}},
			want: "exactly one of claimName",
		},
	} {
		t.Run(name, func(t *testing.T) {
			admitted(t, c, &simplyblockv1alpha2.StorageBackup{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-", Namespace: "default"}, Spec: tc.spec,
			}, tc.want)
		})
	}
}

// A restore names exactly one target: one backup, one group backup by id, or
// the group request that produced one.
func TestStorageBackupOpsNamesExactlyOneTarget(t *testing.T) {
	c := celClient(t)

	ref := simplyblockv1alpha2.NamespacedReference{Name: "a-backup"}
	groupRef := simplyblockv1alpha2.NamespacedReference{Name: "orders-now"}
	for name, tc := range map[string]struct {
		backupRef, groupBackupRef simplyblockv1alpha2.NamespacedReference
		groupBackupID             string
		want                      string
	}{
		"a backup":                   {backupRef: ref},
		"a group backup by id":       {groupBackupID: "gb-1"},
		"a group request":            {groupBackupRef: groupRef},
		"a backup and a group ref":   {backupRef: ref, groupBackupRef: groupRef, want: "exactly one of backupRef"},
		"a group id and a group ref": {groupBackupID: "gb-1", groupBackupRef: groupRef, want: "exactly one of backupRef"},
		"nothing":                    {want: "exactly one of backupRef"},
	} {
		t.Run(name, func(t *testing.T) {
			admitted(t, c, &simplyblockv1alpha2.StorageBackupOps{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-", Namespace: "default"},
				Spec: simplyblockv1alpha2.StorageBackupOpsSpec{
					ClusterRef:     simplyblockv1alpha2.NamespacedDnsReference{Name: "production"},
					BackupRef:      tc.backupRef,
					GroupBackupRef: tc.groupBackupRef,
					GroupBackupID:  tc.groupBackupID,
					Action:         simplyblockv1alpha2.StorageBackupOpsActionRestore,
					Restore:        &simplyblockv1alpha2.RestoreSpec{ClaimName: "restored", TargetPool: "pool"},
				},
			}, tc.want)
		})
	}
}
