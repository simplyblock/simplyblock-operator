// The health of a pNFS volume as its NFSExport reports it. The operator records
// on the export's MDSReady condition when the metadata server does not serve
// it, which the client's own mount does not show.
package node

import (
	"context"
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog"
)

var nfsExportGVR = schema.GroupVersionResource{
	Group: "storage.simplyblock.io", Version: "v1alpha2", Resource: "nfsexports",
}

// nfsSuperMagic is the statfs type of an NFS mount, which is how a pNFS
// volume's mount is told from a block volume's filesystem.
const nfsSuperMagic = 0x6969

// isNFSMount is a variable so a test can take a plain directory for an NFS mount.
// The type is int64 on Linux and uint32 on darwin; uint64 holds both.
var isNFSMount = func(s *unix.Statfs_t) bool { return uint64(s.Type) == nfsSuperMagic }

// SetExportReader gives the node the client it reads NFSExports with. Without
// one, a pNFS volume's condition comes from its mount alone.
func (ns *Server) SetExportReader(client dynamic.Interface) {
	ns.exports = client
}

// exportCondition is the VolumeCondition of the pNFS volume volID: abnormal
// while its NFSExport's MDSReady is False. A missing record, a missing
// condition, or a failed read reports nothing wrong, because none of them says
// the export is not served.
func (ns *Server) exportCondition(ctx context.Context, volID string) *csi.VolumeCondition {
	healthy := &csi.VolumeCondition{}
	if ns.exports == nil {
		return healthy
	}
	spec, ok := backingVolumeOf(volID)
	if !ok {
		return healthy
	}
	// Listed across namespaces: the request names no namespace, and the
	// record's name is unique because it carries the volume's UUID.
	name := "nfsexp-" + spec.VolumeUUID
	listed, err := ns.exports.Resource(nfsExportGVR).Namespace(metav1.NamespaceAll).
		List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
	if err != nil {
		klog.Warningf("reading export %s for volume %s: %v", name, volID, err)
		return healthy
	}
	for _, item := range listed.Items {
		if item.GetName() != name {
			continue
		}
		if status, message, found := mdsReady(&item); found && status == string(metav1.ConditionFalse) {
			return &csi.VolumeCondition{
				Abnormal: true,
				Message:  fmt.Sprintf("pNFS export %s/%s is not served: %s", item.GetNamespace(), name, message),
			}
		}
	}
	return healthy
}

// mdsReady reads the MDSReady condition off an NFSExport.
func mdsReady(export *unstructured.Unstructured) (status, message string, found bool) {
	conditions, _, _ := unstructured.NestedSlice(export.Object, "status", "conditions")
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok || cond["type"] != "MDSReady" {
			continue
		}
		status, _ = cond["status"].(string)
		message, _ = cond["message"].(string)
		return status, message, true
	}
	return "", "", false
}
