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

	corev1 "k8s.io/api/core/v1"

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

// Node affinity is a data-plane placement policy the control plane bakes in at
// cluster create, so the document is the only place it can be stated at all: the
// field is immutable on the cluster the expansion writes.
func TestNodeAffinityReachesTheCluster(t *testing.T) {
	cluster := builtCluster(t, aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.EnableNodeAffinity = ptr.To(true)
	}))

	if got := cluster.Spec.EnableNodeAffinity; got == nil || !*got {
		t.Errorf("enableNodeAffinity = %v, want the document's true", got)
	}
}

// A document that states nothing leaves the cluster stating nothing, so the
// cluster's own default decides rather than the expansion inventing a false the
// control plane would then bake in.
func TestADocumentStatingNoNodeAffinityLeavesTheClusterUnset(t *testing.T) {
	cluster := builtCluster(t, aDocument(func(*simplyblockv1alpha2.ClusterDeploymentConfig) {}))

	if got := cluster.Spec.EnableNodeAffinity; got != nil {
		t.Errorf("enableNodeAffinity = %v with nothing stated, want unset", *got)
	}
}

// The ports every node binds are fixed when the cluster is created and are
// three fields on it, so the document states them as one block and the
// expansion spends it onto the three.
func TestTheDocumentsPortsReachTheCluster(t *testing.T) {
	cluster := builtCluster(t, aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.Ports = &simplyblockv1alpha2.ClusterPortsSpec{
			NVMf:      ptr.To(int32(4430)),
			Rpc:       ptr.To(int32(8090)),
			NodeAgent: ptr.To(int32(50011)),
		}
	}))

	if got := ptr.IntFromOrZero(cluster.Spec.NvmfBasePort); got != 4430 {
		t.Errorf("the NVMe-oF base port is %d, want the document's 4430", got)
	}
	if got := ptr.IntFromOrZero(cluster.Spec.RpcBasePort); got != 8090 {
		t.Errorf("the RPC base port is %d, want the document's 8090", got)
	}
	if got := ptr.IntFromOrZero(cluster.Spec.SnodeApiPort); got != 50011 {
		t.Errorf("the node agent's port is %d, want the document's 50011", got)
	}
}

// The block is spent member by member rather than as a whole, so a member the
// apiserver did not default and nobody stated travels as nothing rather than as
// a zero. What a stored document's block actually holds is all three, because
// the schema defaults the two nobody stated: see
// TestThePortsBlockIsDefaultedByTheApiserver.
func TestThePortsBlockIsSpentMemberByMember(t *testing.T) {
	cluster := builtCluster(t, aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.Ports = &simplyblockv1alpha2.ClusterPortsSpec{NVMf: ptr.To(int32(4430))}
	}))

	if got := ptr.IntFromOrZero(cluster.Spec.NvmfBasePort); got != 4430 {
		t.Errorf("the NVMe-oF base port is %d, want the document's 4430", got)
	}
	if cluster.Spec.RpcBasePort != nil || cluster.Spec.SnodeApiPort != nil {
		t.Errorf("the unstated ports are %v and %v, want both unset",
			cluster.Spec.RpcBasePort, cluster.Spec.SnodeApiPort)
	}
}

// A document with no ports block leaves all three unset, so the cluster's own
// defaults decide.
func TestADocumentWithNoPortsLeavesTheClustersUnset(t *testing.T) {
	cluster := builtCluster(t, aDocument(func(*simplyblockv1alpha2.ClusterDeploymentConfig) {}))

	if cluster.Spec.NvmfBasePort != nil || cluster.Spec.RpcBasePort != nil ||
		cluster.Spec.SnodeApiPort != nil {
		t.Errorf("the ports are %v, %v and %v with nothing stated",
			cluster.Spec.NvmfBasePort, cluster.Spec.RpcBasePort, cluster.Spec.SnodeApiPort)
	}
}

// Where a cluster's backups live is the document's to state, for the reason the
// key store is: the cluster is created from the template and read back on the
// next pass, so a store stated here is present at creation rather than patched
// in afterward by whoever remembers.
func TestTheDocumentsBackupStoreReachesTheCluster(t *testing.T) {
	cluster := builtCluster(t, aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.Backup = &simplyblockv1alpha2.BackupStoreSpec{
			Endpoint:             "https://s3.example.com",
			Bucket:               "simplyblock-backups",
			Prefix:               "production/",
			Region:               "eu-central-1",
			CredentialsSecretRef: corev1.LocalObjectReference{Name: "backup-credentials"},
		}
	}))

	store := cluster.Spec.Backup
	if store == nil {
		t.Fatal("the cluster carries no backup store")
	}
	if store.Bucket != "simplyblock-backups" || store.Prefix != "production/" {
		t.Errorf("the store is %+v, want the document's bucket and prefix", store)
	}
	if store.Endpoint != "https://s3.example.com" || store.Region != "eu-central-1" {
		t.Errorf("the store is %+v, want the document's endpoint and region", store)
	}
	if store.CredentialsSecretRef.Name != "backup-credentials" {
		t.Errorf("the store reads its credentials from %q", store.CredentialsSecretRef.Name)
	}
}

// A document that states no store creates a cluster with none, which is a
// cluster whose backups are nobody's yet: the block is mutable, so it is given
// one whenever there is one to give.
func TestADocumentWithNoBackupStoreCreatesAClusterWithNone(t *testing.T) {
	cluster := builtCluster(t, aDocument(func(*simplyblockv1alpha2.ClusterDeploymentConfig) {}))

	if cluster.Spec.Backup != nil {
		t.Errorf("the cluster carries the store %+v with nothing stated", cluster.Spec.Backup)
	}
}
