// The StorageNode admission guard, which answers two different kinds of question.
//
// On create it resolves spec.clusterRef and checks the node's configuration
// against the cluster it names: whether the devices it lists are of the class the
// cluster is built out of, and whether its sizing agrees with the fleet's. Both
// are properties of the cluster rather than of the request, so neither can be
// decided from the admission review alone.
//
// On update it enforces the fields that have exactly one legitimate writer.
// spec.workerNode, spec.socketId, spec.nodeIndex, spec.config.pcieAllowList, and
// spec.config.sizing are all written by the operator and by nobody else, so a
// +k8s:immutable marker would lock the operator out along with everyone else and
// no marker at all would let a user invalidate a layout claim by editing a
// string.
//
// failurePolicy=Fail, and that is safe because the webhook server runs in the
// operator pod: its availability tracks the operator's own, and an operator that
// is down is not reconciling anything the rejection could deadlock. The guarded
// fields have no CRD-level immutability behind them, so this is their only guard
// and admitting while unavailable would let the edit through.
//
// design-storagenode.md §3.2 and §3.4 are the specification.

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// matchPolicy=Equivalent is load-bearing rather than a default worth inheriting.
// The CRD serves v1alpha1 as well, and under the API server's default Exact policy
// a rule naming only v1alpha2 does not see a v1alpha1 write at all — so a client
// writing the older version would place a node naming a cluster that does not
// exist, or re-point a worker this guard exists to hold.

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storagenode,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,sideEffects=None,groups=storage.simplyblock.io,resources=storagenodes,verbs=create;update,versions=v1alpha2,name=vstoragenode.simplyblock.io,admissionReviewVersions=v1

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch

// StorageNodeValidator admits a StorageNode write.
type StorageNodeValidator struct {
	// Client reads the StorageCluster the node names.
	Client client.Client

	// OperatorNamespace is the namespace the operator runs in. A service account
	// in it is the operator itself, and is the one identity permitted to write the
	// guarded fields of §3.2.
	OperatorNamespace string
}

func (v *StorageNodeValidator) Handle(
	ctx context.Context, req admission.Request,
) admission.Response {
	switch req.Operation {
	case admissionv1.Create:
		var node simplyblockv1alpha2.StorageNode
		if err := json.Unmarshal(req.Object.Raw, &node); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		return v.admitCreate(ctx, &node, v.isOperator(req))

	case admissionv1.Update:
		var oldNode, newNode simplyblockv1alpha2.StorageNode
		if err := json.Unmarshal(req.OldObject.Raw, &oldNode); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if err := json.Unmarshal(req.Object.Raw, &newNode); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		return v.admitUpdate(&oldNode, &newNode, v.isOperator(req))

	default:
		return admission.Allowed("")
	}
}

// admitCreate resolves the cluster and checks the node against it.
func (v *StorageNodeValidator) admitCreate(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, byOperator bool,
) admission.Response {
	// The reference is immutable from creation, which is what makes admission the
	// right place. A node naming a cluster that is not there can never be
	// corrected — the field cannot be edited, so the only remedy is to delete the
	// object and write it again, which is exactly what this rejection asks for.
	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: node.Namespace, Name: node.Spec.ClusterRef}
	switch err := v.Client.Get(ctx, key, &cluster); {
	case apierrors.IsNotFound(err):
		return admission.Denied(fmt.Sprintf(
			"spec.clusterRef names StorageCluster %q, which does not exist in namespace %q",
			node.Spec.ClusterRef, node.Namespace))
	case err != nil:
		return admission.Errored(http.StatusInternalServerError, err)
	}

	if denial := deviceClassDenial(&node.Spec.Config, &cluster); denial != "" {
		return admission.Denied(denial)
	}

	// The operator writes a node's sizing itself, and a rolling hardware upgrade
	// is the case where it deliberately differs from the fleet's. A user's node is
	// held to the cluster's values, because unmanaged divergence is what stops the
	// control plane placing erasure-coding chunks evenly (§3.1).
	if !byOperator {
		if denial := sizingDenial(&node.Spec.Config.Sizing, &cluster); denial != "" {
			return admission.Denied(denial)
		}
	}
	return admission.Allowed("")
}

// admitUpdate enforces the fields with exactly one legitimate writer.
func (v *StorageNodeValidator) admitUpdate(
	oldNode, newNode *simplyblockv1alpha2.StorageNode, byOperator bool,
) admission.Response {
	changed := operatorOnlyChanges(oldNode, newNode)
	if len(changed) == 0 {
		return admission.Allowed("")
	}
	if byOperator {
		return admission.Allowed("operator-driven change to " + strings.Join(changed, ", "))
	}
	return admission.Denied(fmt.Sprintf(
		"%s %s written by the operator alone; "+
			"relocate a node with a StorageNodeOps of action Migrate, and let a "+
			"re-size follow the fleet rather than editing it here",
		strings.Join(changed, " and "), plural(len(changed), "is", "are")))
}

// operatorOnlyChanges names the guarded fields this update would change.
func operatorOnlyChanges(oldNode, newNode *simplyblockv1alpha2.StorageNode) []string {
	var changed []string
	if oldNode.Spec.WorkerNode != newNode.Spec.WorkerNode {
		changed = append(changed, "spec.workerNode")
	}
	// Where a node sits on its host, which a relocation changes along with the
	// host: the target's free socket is not necessarily the source's. They are
	// guarded rather than frozen for the reason workerNode is, and apart from
	// spec.slot, which stays frozen because the topology label the CSI driver
	// reads is built from it.
	if oldNode.Spec.SocketID != newNode.Spec.SocketID {
		changed = append(changed, "spec.socketId")
	}
	if !equalIndex(oldNode.Spec.NodeIndex, newNode.Spec.NodeIndex) {
		changed = append(changed, "spec.nodeIndex")
	}
	// The allow list is the one device field a migration writes, merging the
	// drives bound on the target host into it so they survive a later rebuild
	// (§3.2). That is why it is guarded here rather than marked immutable.
	if !equalStrings(oldNode.Spec.Config.PcieAllowList, newNode.Spec.Config.PcieAllowList) {
		changed = append(changed, "spec.config.pcieAllowList")
	}
	if !equalSizing(oldNode.Spec.Config.Sizing, newNode.Spec.Config.Sizing) {
		changed = append(changed, "spec.config.sizing")
	}
	return changed
}

// equalIndex compares two node indexes, unset included: an index that was absent
// and one that is zero are different states, and reading both as zero would admit
// the write that states it.
func equalIndex(old, new *int32) bool {
	if old == nil || new == nil {
		return old == new
	}
	return *old == *new
}

// deviceClassDenial reports why a node's device configuration does not belong to
// its cluster's class, or the empty string when it does.
//
// A cluster is built out of one class of backend storage, because an
// erasure-coding stripe placed across both is written and rebuilt at the slower
// one's rate (§3.1). So every entry of a node's list is of its cluster's class,
// a list holding both is rejected, and so is a list of the class the cluster is
// not.
func deviceClassDenial(
	config *simplyblockv1alpha2.StorageNodeConfig, cluster *simplyblockv1alpha2.StorageCluster,
) string {
	// An unstated class is NVMe, which is what the CRD defaults it to and what
	// describes every cluster that predates the field.
	class := cluster.Spec.DeviceClass
	if class == "" {
		class = simplyblockv1alpha2.StorageClusterDeviceClassNVMe
	}

	var addresses, paths []string
	for _, name := range config.DeviceNames {
		if pciAddress.MatchString(name) {
			addresses = append(addresses, name)
			continue
		}
		paths = append(paths, name)
	}

	if len(addresses) > 0 && len(paths) > 0 {
		return fmt.Sprintf(
			"spec.config.deviceNames mixes PCI addresses (%s) with device paths (%s); "+
				"a cluster is built out of one class of backend storage",
			strings.Join(addresses, ", "), strings.Join(paths, ", "))
	}

	switch class {
	case simplyblockv1alpha2.StorageClusterDeviceClassNVMe:
		if len(paths) > 0 {
			return fmt.Sprintf(
				"spec.config.deviceNames holds device paths (%s) and cluster %q has "+
					"deviceClass NVMe, whose devices are named by PCI address",
				strings.Join(paths, ", "), cluster.Name)
		}

	case simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock:
		if len(addresses) > 0 {
			return fmt.Sprintf(
				"spec.config.deviceNames holds PCI addresses (%s) and cluster %q has "+
					"deviceClass LogicalBlock, whose devices are named by path",
				strings.Join(addresses, ", "), cluster.Name)
		}
		// The PCI filters match on something a logical block device does not have,
		// so they are rejected rather than ignored: a filter that silently selects
		// nothing is a node that comes up with no devices and no reason given.
		var stated []string
		if len(config.PcieAllowList) > 0 {
			stated = append(stated, "spec.config.pcieAllowList")
		}
		if len(config.PcieDenyList) > 0 {
			stated = append(stated, "spec.config.pcieDenyList")
		}
		if config.PcieModel != "" {
			stated = append(stated, "spec.config.pcieModel")
		}
		if len(stated) > 0 {
			return fmt.Sprintf(
				"%s %s stated and cluster %q has deviceClass LogicalBlock, whose "+
					"devices have no PCI address to match",
				strings.Join(stated, " and "), plural(len(stated), "is", "are"), cluster.Name)
		}
	}
	return ""
}

// sizingDenial reports why a node's sizing disagrees with its cluster's, or the
// empty string when it agrees.
//
// Both values are stated once, on the cluster, and a node holds a stamp of what it
// was built with rather than a number somebody chose for it. A fleet whose nodes
// differ is a fleet mid-roll, never a fleet somebody described that way.
func sizingDenial(
	sizing *simplyblockv1alpha2.StorageNodeSizing, cluster *simplyblockv1alpha2.StorageCluster,
) string {
	if want := cluster.Spec.VCPUCount; want != nil {
		if sizing.VCPUCount == nil || *sizing.VCPUCount != *want {
			return fmt.Sprintf(
				"spec.config.sizing.vcpuCount is %s and cluster %q states %d; "+
					"a node's sizing is stamped from its cluster, and a roll that "+
					"changes it is the operator's to perform",
				describeCount(sizing.VCPUCount), cluster.Name, *want)
		}
	}
	if want := cluster.Spec.MinHugePagesSize; want != "" && sizing.MinHugePagesSize != want {
		return fmt.Sprintf(
			"spec.config.sizing.minHugePagesSize is %q and cluster %q states %q",
			sizing.MinHugePagesSize, cluster.Name, want)
	}
	return ""
}

// isOperator reports whether the request came from a service account in the
// operator's own namespace, which is the operator itself.
func (v *StorageNodeValidator) isOperator(req admission.Request) bool {
	return strings.HasPrefix(req.UserInfo.Username,
		"system:serviceaccount:"+v.OperatorNamespace+":")
}

// pciAddress matches the domain:bus:device.function form a PCI address takes. It
// is the same expression the type's own items pattern carries for that half of
// the union, so the two cannot disagree about what an address looks like.
var pciAddress = regexp.MustCompile(
	`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`)

// describeCount renders a count for a message, so an absent one reads as absent
// rather than as zero.
func describeCount(count *int32) string {
	if count == nil {
		return "unset"
	}
	return fmt.Sprintf("%d", *count)
}

// plural picks the verb for a count, so a message reads "is stated" for one field
// and "are stated" for several.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// equalSizing compares two sizing blocks by value.
//
// It is spelled out rather than compared with ==, because the core count is a
// pointer and two separately decoded objects hold two pointers to the same
// number. Comparing the structs would read every update as a re-size and refuse
// every edit a user is entitled to make.
func equalSizing(a, b simplyblockv1alpha2.StorageNodeSizing) bool {
	if a.MinHugePagesSize != b.MinHugePagesSize {
		return false
	}
	if a.VCPUCount == nil || b.VCPUCount == nil {
		return a.VCPUCount == b.VCPUCount
	}
	return *a.VCPUCount == *b.VCPUCount
}

// equalStrings compares two lists for the purpose of deciding whether an update
// changed one. Order matters: the allow list is a user's, and a reordering is a
// write of the field whoever did it has to be allowed to make.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
