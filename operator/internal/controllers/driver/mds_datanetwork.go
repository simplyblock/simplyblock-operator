// The metadata server's attachment to the storage data network: the Multus
// NetworkAttachmentDefinition the operator keeps for it, and the choice of the
// host interface its macvlan sits on.
//
// The storage nodes' NVMe/TCP listeners are bound to their data interfaces, so
// a connection reaches one only when it arrives on that interface. From the pod
// network it arrives there only when the node has forwarding enabled on the
// data interface, which Talos and K3s have and OpenShift's OVN-Kubernetes does
// not by default. A macvlan on the data interface puts the pod's traffic on the data
// network itself. Multus is not on every cluster, so the attachment is opt-in
// through spec.pnfs.mds.dataNetwork, and a cluster that cannot provide it runs
// the metadata server on the pod network as before, with the reason reported.

package driver

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/multus"
)

const (
	// MDSDataRangeAnnotation records the address range of the data network on
	// the metadata server's pod template. The range lives in the
	// NetworkAttachmentDefinition, which Multus reads only when a pod starts,
	// so recording it here is what makes a range edit restart the pod.
	MDSDataRangeAnnotation = "storage.simplyblock.io/mds-data-range"

	// MDSDataInterface is the name the data-network interface takes in the
	// metadata server pod.
	MDSDataInterface = "net1"

	// ReasonMDSDataNetworkUnavailable is the event reason of a data network
	// the cluster cannot attach: no Multus.
	ReasonMDSDataNetworkUnavailable = "MDSDataNetworkUnavailable"

	// ReasonNoDataNetworkNode is the event reason of a data network no node
	// the metadata server can run on is attached to.
	ReasonNoDataNetworkNode = "NoDataNetworkNode"
)

// MDSDataAttachment is how the metadata server pod joins the data network: the
// NetworkAttachmentDefinition, and the host interface its macvlan sits on,
// which the pod can only be scheduled where it exists.
type MDSDataAttachment struct {
	Network   string
	Interface string
}

// MDSDataNetworkFallback says why the data network the spec names is not
// attached, so the metadata server runs on the pod network instead. It is
// returned as an error for the caller to report, never to stop on.
type MDSDataNetworkFallback struct{ Reason, Message string }

func (f *MDSDataNetworkFallback) Error() string { return f.Message }

// MDSDataNetworkName is the driver's NetworkAttachmentDefinition, shared by the
// metadata servers of all its storage clusters.
func MDSDataNetworkName(d *simplyblockv1alpha2.SimplyblockDriver) string {
	return names(d).prefix + "mds-data"
}

// EnsureMDSDataNetwork keeps the driver's NetworkAttachmentDefinition in line
// with spec.pnfs.mds.dataNetwork and returns the attachment the metadata server
// pods use. It returns no attachment and no error when the spec names no data
// network, and no attachment and an *MDSDataNetworkFallback when the cluster
// cannot attach the one it names.
//
// The host interface is the one most of the nodes the pod can run on carry the
// subnet on, read from their subnet labels. Once chosen it is kept while any of
// those nodes still carries it: choosing again changes the pod template, and
// that restarts the guest.
func EnsureMDSDataNetwork(
	ctx context.Context, c client.Client, d *simplyblockv1alpha2.SimplyblockDriver,
) (*MDSDataAttachment, error) {
	if d.Spec.PNFS.MDS == nil || d.Spec.PNFS.MDS.DataNetwork == nil {
		return nil, nil
	}
	network := d.Spec.PNFS.MDS.DataNetwork
	subnet, err := netip.ParsePrefix(network.Subnet)
	if err != nil {
		return nil, &MDSDataNetworkFallback{Reason: ReasonNoDataNetworkNode,
			Message: fmt.Sprintf("spec.pnfs.mds.dataNetwork.subnet %q is not a CIDR", network.Subnet)}
	}

	nad := multus.NewDefinition()
	key := client.ObjectKey{Namespace: d.Namespace, Name: MDSDataNetworkName(d)}
	found := true
	switch err := c.Get(ctx, key, nad); {
	case multus.KindNotServed(err):
		return nil, &MDSDataNetworkFallback{Reason: ReasonMDSDataNetworkUnavailable,
			Message: "spec.pnfs.mds.dataNetwork is set, but the cluster has no NetworkAttachmentDefinition " +
				"kind (Multus), so the metadata server reaches the storage nodes through the pod network"}
	case apierrors.IsNotFound(err):
		found = false
	case err != nil:
		return nil, fmt.Errorf("reading the NetworkAttachmentDefinition %s: %w", key.Name, err)
	}

	carriers, err := subnetInterfaces(ctx, c, d.Spec.PNFS.MDS, subnet)
	if err != nil {
		return nil, err
	}
	iface := ""
	if found {
		if current := nadMaster(nad); carriers[current] > 0 {
			iface = current
		}
	}
	if iface == "" {
		iface = mostCommon(carriers)
	}
	if iface == "" {
		return nil, &MDSDataNetworkFallback{Reason: ReasonNoDataNetworkNode, Message: fmt.Sprintf(
			"no node the metadata server can run on has an interface on %s, so it reaches the storage "+
				"nodes through the pod network", subnet)}
	}

	config, err := json.Marshal(map[string]any{
		"cniVersion": "0.3.1",
		"name":       key.Name,
		"type":       "macvlan",
		"master":     iface,
		"mode":       "bridge",
		"ipam": map[string]any{
			"type":        "whereabouts",
			"range":       subnet.Masked().String(),
			"range_start": network.RangeStart,
			"range_end":   network.RangeEnd,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encoding the NetworkAttachmentDefinition config: %w", err)
	}

	if !found {
		nad.SetNamespace(key.Namespace)
		nad.SetName(key.Name)
	}
	current, _, _ := unstructured.NestedString(nad.Object, "spec", "config")
	if found && current == string(config) {
		return &MDSDataAttachment{Network: key.Name, Interface: iface}, nil
	}
	if err := unstructured.SetNestedField(nad.Object, string(config), "spec", "config"); err != nil {
		return nil, fmt.Errorf("setting the NetworkAttachmentDefinition config: %w", err)
	}
	if err := controllerutil.SetControllerReference(d, nad, c.Scheme()); err != nil {
		return nil, fmt.Errorf("owning the NetworkAttachmentDefinition %s by the driver: %w", key.Name, err)
	}
	if found {
		err = c.Update(ctx, nad)
	} else {
		err = c.Create(ctx, nad)
	}
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("writing the NetworkAttachmentDefinition %s: %w", key.Name, err)
	}
	return &MDSDataAttachment{Network: key.Name, Interface: iface}, nil
}

// subnetInterfaces counts, per host interface, the nodes the metadata server
// can run on that carry subnet on it: the KVM-capable nodes matching the spec's
// node selector.
func subnetInterfaces(
	ctx context.Context, c client.Client, spec *simplyblockv1alpha2.DriverPNFSMDS, subnet netip.Prefix,
) (map[string]int, error) {
	selector := client.MatchingLabels{KVMCapableLabel: KVMCapableValue}
	for k, v := range spec.NodeSelector {
		selector[k] = v
	}
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes, selector, client.HasLabels{kube.SubnetLabel(subnet)}); err != nil {
		return nil, fmt.Errorf("listing the nodes on %s: %w", subnet, err)
	}
	counts := map[string]int{}
	for _, node := range nodes.Items {
		if iface := node.Labels[kube.SubnetLabel(subnet)]; iface != "" {
			counts[iface]++
		}
	}
	return counts, nil
}

// mostCommon is the interface with the highest count, the first by name on a
// tie, so that the same nodes always give the same answer.
func mostCommon(counts map[string]int) string {
	ifaces := make([]string, 0, len(counts))
	for iface := range counts {
		ifaces = append(ifaces, iface)
	}
	slices.SortFunc(ifaces, func(a, b string) int {
		return cmp.Or(cmp.Compare(counts[b], counts[a]), cmp.Compare(a, b))
	})
	if len(ifaces) == 0 {
		return ""
	}
	return ifaces[0]
}

// nadMaster is the host interface a NetworkAttachmentDefinition's macvlan sits
// on, or empty when its config names none.
func nadMaster(nad *unstructured.Unstructured) string {
	raw, _, _ := unstructured.NestedString(nad.Object, "spec", "config")
	var config struct {
		Master string `json:"master"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return ""
	}
	return config.Master
}

// dataNetworkPodSpec attaches the pod to the data network: the Multus
// annotation, a required node affinity on the attachment's interface carrying
// the subnet, and a preference for nodes that run no storage node. The path
// from a macvlan to its own host's address does not work, so on a storage node
// the metadata server would lose the path to that node. The address range is
// recorded too, so a change to it restarts the pod.
func dataNetworkPodSpec(
	template *corev1.PodTemplateSpec, subnet netip.Prefix, network *simplyblockv1alpha2.DriverPNFSDataNetwork,
	data *MDSDataAttachment,
) error {
	networks, err := json.Marshal([]map[string]string{{"name": data.Network, "interface": MDSDataInterface}})
	if err != nil {
		return fmt.Errorf("encoding the networks annotation: %w", err)
	}
	template.Annotations[multus.NetworksAnnotation] = string(networks)
	template.Annotations[MDSDataRangeAnnotation] = network.RangeStart + "-" + network.RangeEnd
	template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key: kube.SubnetLabel(subnet), Operator: corev1.NodeSelectorOpIn, Values: []string{data.Interface},
			}}}},
		},
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
			Weight: 100,
			Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key: kube.LabelStorageNodeSet, Operator: corev1.NodeSelectorOpDoesNotExist,
			}}},
		}},
	}}
	return nil
}
