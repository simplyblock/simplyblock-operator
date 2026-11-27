// Which spec fields of the replication kinds an update may change, checked by a
// real apiserver because only one evaluates the CRDs' transition rules.
//
// Regression: simplyblock-operator#260 — the fields below were documented as
// immutable and enforced by nothing. The ReplicationOps webhook answers creates
// only, so an edit to a running operation's action, scope, target, or
// deleteSource was admitted and followed part way through.

package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

// celEnvtestAssets is KUBEBUILDER_ASSETS, or the first version under the
// repository's .bin/k8s, which is what lets the test run from an editor.
func celEnvtestAssets() string {
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	base := filepath.Join("..", "..", "..", ".bin", "k8s")
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

// replicationCELClient starts a test apiserver with this repository's CRDs.
func replicationCELClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: celEnvtestAssets(),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start the test apiserver: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// updated creates obj, applies edit to it, and checks the apiserver's answer to
// the update: admitted when want is empty, otherwise rejected with a message
// containing want.
func updated[T client.Object](t *testing.T, c client.Client, obj T, edit func(T), want string) {
	t.Helper()
	ctx := context.Background()
	if err := c.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, obj) })
	edit(obj)
	err := c.Update(ctx, obj)
	switch {
	case want == "" && err != nil:
		t.Errorf("rejected: %v", err)
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		t.Errorf("err = %v, want a rejection mentioning %q", err, want)
	}
}

func replObjectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: "default"}
}

func TestAReplicationOpsSpecCannotChangeOnceCreated(t *testing.T) {
	c := replicationCELClient(t)

	failback := func(name string) *simplyblockv1alpha1.ReplicationOps {
		return &simplyblockv1alpha1.ReplicationOps{
			ObjectMeta: replObjectMeta(name),
			Spec: simplyblockv1alpha1.ReplicationOpsSpec{
				Action: "failback", Scope: "policy", Ref: "policy-a",
			},
		}
	}
	for name, edit := range map[string]func(*simplyblockv1alpha1.ReplicationOps){
		"action": func(o *simplyblockv1alpha1.ReplicationOps) { o.Spec.Action = "trigger" },
		"scope":  func(o *simplyblockv1alpha1.ReplicationOps) { o.Spec.Scope = "target" },
		"ref":    func(o *simplyblockv1alpha1.ReplicationOps) { o.Spec.Ref = "policy-b" },
		"sourceClusterID": func(o *simplyblockv1alpha1.ReplicationOps) {
			o.Spec.SourceClusterID = "f00dfeed-0000-4000-8000-000000000001"
		},
		// Omitted while false, so it is set rather than changed: a rule that
		// guarded only a value already present would admit it.
		"deleteSource": func(o *simplyblockv1alpha1.ReplicationOps) { o.Spec.DeleteSource = true },
	} {
		t.Run(name, func(t *testing.T) {
			updated(t, c, failback("ops-"+strings.ToLower(name)), edit, "immutable")
		})
	}

	t.Run("metadata", func(t *testing.T) {
		updated(t, c, failback("ops-labels"), func(o *simplyblockv1alpha1.ReplicationOps) {
			o.Labels = map[string]string{"team": "storage"}
		}, "")
	})
}

func TestAReplicationSlotKeepsItsPolicyAndClaim(t *testing.T) {
	c := replicationCELClient(t)

	slot := func(name string) *simplyblockv1alpha1.ReplicationSlot {
		return &simplyblockv1alpha1.ReplicationSlot{
			ObjectMeta: replObjectMeta(name),
			Spec: simplyblockv1alpha1.ReplicationSlotSpec{
				PolicyRef: "policy-a", PVCRef: "claim-a",
				VolumeID: "c0ffee00-0000-4000-8000-000000000001:pool:vol-old",
			},
		}
	}
	t.Run("policyRef", func(t *testing.T) {
		updated(t, c, slot("slot-policy"), func(s *simplyblockv1alpha1.ReplicationSlot) {
			s.Spec.PolicyRef = "policy-b"
		}, "immutable")
	})
	t.Run("pvcRef", func(t *testing.T) {
		updated(t, c, slot("slot-pvc"), func(s *simplyblockv1alpha1.ReplicationSlot) {
			s.Spec.PVCRef = "claim-b"
		}, "immutable")
	})
	// The operator repoints a slot at the volume serving its data after a
	// migration (followMovedVolume), so volumeID stays writable.
	t.Run("volumeID follows a moved volume", func(t *testing.T) {
		updated(t, c, slot("slot-volume"), func(s *simplyblockv1alpha1.ReplicationSlot) {
			s.Spec.VolumeID = "c0ffee00-0000-4000-8000-000000000002:pool:vol-new"
		}, "")
	})
}

func TestAReplicationPairKeepsItsSourceCluster(t *testing.T) {
	c := replicationCELClient(t)

	updated(t, c, &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: replObjectMeta("pair"),
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			SourceCluster: "cluster-a", TargetCluster: "cluster-b",
		},
	}, func(p *simplyblockv1alpha1.ReplicationPair) {
		p.Spec.SourceCluster = "cluster-c"
	}, "immutable")
}
