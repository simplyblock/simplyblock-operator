// Where the document's drive-format decision lands, which differs by class.
//
// enableDriveFormat says what is wanted and not how, because the how is not the
// same thing twice: an NVMe device is reformatted to a 4K block size by the
// control plane at node-add, and a logical block device has its signatures
// wiped locally by node_configure.py before the node is added at all. Two
// operations, two channels, one field on the document -- so the expansion is
// where the one becomes the other.

package deployment

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aBlockDocument is aDocument with its group naming a block device, which is
// what makes DeviceClassOf read the cluster as the block class.
func aBlockDocument(format bool) *simplyblockv1alpha2.ClusterDeploymentConfig {
	return aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.EnableDriveFormat = ptr.To(format)
		c.Spec.NodeSets[0].Groups[0].Devices = &simplyblockv1alpha2.DeviceSelection{
			Block: []string{"/dev/sdb"},
		}
	})
}

func TestABlockDeploymentFormatsThroughTheBlockChannel(t *testing.T) {
	cluster := builtCluster(t, aBlockDocument(true))

	workload := cluster.Spec.StorageNodes
	if workload == nil {
		t.Fatal("the cluster carries no storage-node workload")
	}
	if got := workload.EnableBlockFormat; got == nil || !*got {
		t.Errorf("EnableBlockFormat = %v, and wipefs is how a block device is formatted", got)
	}
	// And not through the other one: format_4k asks the control plane to
	// reformat an NVMe namespace, which this cluster has none of.
	if got := workload.EnableFormat4K; got != nil && *got {
		t.Error("a block cluster asked the control plane to reformat an NVMe namespace")
	}
}

func TestAnNVMeDeploymentStillFormatsThroughItsOwnChannel(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.EnableDriveFormat = ptr.To(true)
	})

	workload := builtCluster(t, config).Spec.StorageNodes
	if got := workload.EnableFormat4K; got == nil || !*got {
		t.Errorf("EnableFormat4K = %v", got)
	}
	if got := workload.EnableBlockFormat; got != nil && *got {
		t.Error("an NVMe cluster asked for a block device's signatures to be wiped")
	}
}

// A document that does not ask formats nothing, in either class. The setting is
// destructive, so an unstated one is off rather than defaulted.
func TestADocumentThatDoesNotAskFormatsNothing(t *testing.T) {
	workload := builtCluster(t, aBlockDocument(false)).Spec.StorageNodes

	if got := workload.EnableBlockFormat; got != nil && *got {
		t.Error("a document that declined to format wiped the drives anyway")
	}
	if got := workload.EnableFormat4K; got != nil && *got {
		t.Error("a document that declined to format reformatted the drives anyway")
	}
}
