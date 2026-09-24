// The integrity settings a document has to be able to state.
//
// Both are immutable on the StorageCluster, so a cluster an expansion created
// without them is a cluster nobody can turn them on for afterward. Leaving
// them off ClusterTemplate therefore did not default a deployment to no
// checksums; it made checksums unreachable for every cluster a document
// produces, which is the whole of the CRD path.

package deployment

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestADocumentStatesChecksumValidation(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.EnableChecksumValidation = ptr.To(true)
	})

	cluster := builtCluster(t, config)

	if got := cluster.Spec.EnableChecksumValidation; got == nil || !*got {
		t.Fatalf("EnableChecksumValidation = %v, and the cluster is immutable, "+
			"so a document that cannot ask is a cluster that never has it", got)
	}
}

func TestADocumentStatesFourKiBAtomicity(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.EnableChecksumValidation = ptr.To(true)
		c.Spec.Cluster.EnableAtomicity4K = ptr.To(true)
	})

	cluster := builtCluster(t, config)

	if got := cluster.Spec.EnableAtomicity4K; got == nil || !*got {
		t.Fatalf("EnableAtomicity4K = %v", got)
	}
}

// A document that states neither leaves both unset rather than false, so the
// cluster's own defaults decide and the expansion invents nothing.
func TestADocumentStatingNeitherLeavesBothToTheCluster(t *testing.T) {
	cluster := builtCluster(t, aDocument(nil))

	if got := cluster.Spec.EnableChecksumValidation; got != nil {
		t.Errorf("EnableChecksumValidation = %v, want unset", *got)
	}
	if got := cluster.Spec.EnableAtomicity4K; got != nil {
		t.Errorf("EnableAtomicity4K = %v, want unset", *got)
	}
}

// KMS is immutable on the StorageCluster too (storagecluster_types.go's kms
// CEL rule), and the expansion's own reconciler reads it back off that object
// on its very next pass — before anything outside the cluster could patch it
// in. So a document that cannot state it is a cluster whose key store can
// never be anything but the cluster's own default.
func TestADocumentStatesKMS(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.KMS = &simplyblockv1alpha2.KMSSpec{
			Vault: &simplyblockv1alpha2.VaultKMS{Endpoint: "https://vault.example.com:8200"},
		}
	})

	cluster := builtCluster(t, config)

	if got := cluster.Spec.KMS; got == nil || got.Vault == nil || got.Vault.Endpoint != "https://vault.example.com:8200" {
		t.Fatalf("KMS = %+v, want the document's vault endpoint carried through", got)
	}
}

// builtCluster is the StorageCluster a document's template describes.
func builtCluster(
	t *testing.T, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) *simplyblockv1alpha2.StorageCluster {
	t.Helper()
	r := reconcilerFor(t, config)
	cluster, err := r.buildCluster(config, config.Spec.Cluster.Name)
	if err != nil {
		t.Fatalf("build the cluster: %v", err)
	}
	return cluster
}
