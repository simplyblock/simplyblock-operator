// The metadata server's attachment to the storage data network: the pod
// template it produces, and the NetworkAttachmentDefinition the operator keeps
// for it, chosen from the subnet labels the node plugin publishes.

package driver

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/multus"
)

const (
	testDataSubnet = "10.10.10.0/24"
	testDataIface  = "ens16np0"
)

func withDataNetwork(d *simplyblockv1alpha2.SimplyblockDriver) *simplyblockv1alpha2.SimplyblockDriver {
	d = withMDS(d)
	d.Spec.PNFS.MDS.DataNetwork = &simplyblockv1alpha2.DriverPNFSDataNetwork{
		Subnet: testDataSubnet, RangeStart: "10.10.10.200", RangeEnd: "10.10.10.209",
	}
	return d
}

func mustPrefix(t *testing.T, cidr string) netip.Prefix {
	t.Helper()
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return prefix
}

func subnetKey(t *testing.T) string {
	t.Helper()
	return kube.SubnetLabel(mustPrefix(t, testDataSubnet))
}

// The pod joins the network through the attachment, runs only where the
// interface exists, prefers a node without a storage node so the path to every
// storage node stays reachable, and tells the runner which interface it is.
func TestMDSPodAttachesToTheDataNetwork(t *testing.T) {
	d := withDataNetwork(testDriver("simplyblock"))
	_, sts, err := MDSObjects(d, testClusterID, "simplyblock-state",
		&MDSDataAttachment{Network: "simplyblock-mds-data", Interface: testDataIface})
	if err != nil {
		t.Fatalf("MDSObjects: %v", err)
	}
	pod := sts.Spec.Template

	var networks []map[string]string
	if err := json.Unmarshal([]byte(pod.Annotations[multus.NetworksAnnotation]), &networks); err != nil {
		t.Fatalf("networks annotation %q: %v", pod.Annotations[multus.NetworksAnnotation], err)
	}
	if len(networks) != 1 || networks[0]["name"] != "simplyblock-mds-data" ||
		networks[0]["interface"] != MDSDataInterface {
		t.Errorf("networks = %v, want simplyblock-mds-data as %s", networks, MDSDataInterface)
	}

	affinity := pod.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil ||
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("affinity = %+v, want a required node affinity", affinity)
	}
	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	want := corev1.NodeSelectorRequirement{
		Key: subnetKey(t), Operator: corev1.NodeSelectorOpIn, Values: []string{testDataIface},
	}
	if len(terms) != 1 || !slices.ContainsFunc(terms[0].MatchExpressions, func(r corev1.NodeSelectorRequirement) bool {
		return r.Key == want.Key && r.Operator == want.Operator && slices.Equal(r.Values, want.Values)
	}) {
		t.Errorf("required terms = %+v, want one requiring %+v", terms, want)
	}
	preferred := affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if !slices.ContainsFunc(preferred, func(p corev1.PreferredSchedulingTerm) bool {
		return slices.ContainsFunc(p.Preference.MatchExpressions, func(r corev1.NodeSelectorRequirement) bool {
			return r.Key == kube.LabelStorageNodeSet && r.Operator == corev1.NodeSelectorOpDoesNotExist
		})
	}) {
		t.Errorf("preferred terms = %+v, want one preferring nodes without %s", preferred, kube.LabelStorageNodeSet)
	}

	if args := runnerContainer(t, sts).Args; !slices.Contains(args, "--data-interface="+MDSDataInterface) {
		t.Errorf("runner args = %v, want --data-interface=%s", args, MDSDataInterface)
	}
}

// Narrowing or moving the address range restarts the metadata server, as the
// API promises: the range lives in the NetworkAttachmentDefinition, which
// Multus reads only when a pod starts, so the running pod would keep an
// address the range no longer holds.
func TestMDSPodTemplateChangesWithTheAddressRange(t *testing.T) {
	attachment := &MDSDataAttachment{Network: "simplyblock-mds-data", Interface: testDataIface}
	hash := func(start, end string) string {
		t.Helper()
		d := withDataNetwork(testDriver("simplyblock"))
		d.Spec.PNFS.MDS.DataNetwork.RangeStart, d.Spec.PNFS.MDS.DataNetwork.RangeEnd = start, end
		_, sts, err := MDSObjects(d, testClusterID, "simplyblock-state", attachment)
		if err != nil {
			t.Fatalf("MDSObjects: %v", err)
		}
		return sts.Annotations[MDSTemplateHashAnnotation]
	}
	was := hash("10.10.10.200", "10.10.10.209")
	if got := hash("10.10.10.200", "10.10.10.204"); got == was {
		t.Error("a new rangeEnd left the template hash as it was, so the pod keeps its address")
	}
	if got := hash("10.10.10.210", "10.10.10.219"); got == was {
		t.Error("a new rangeStart left the template hash as it was, so the pod keeps its address")
	}
}

// Without a data network nothing of the attachment appears, so the pod
// template, and with it the running guest, stays as it was.
func TestMDSPodWithoutADataNetworkHasNoAttachment(t *testing.T) {
	_, sts := mdsObjects(t, withMDS(testDriver("simplyblock")))
	if v, found := sts.Spec.Template.Annotations[multus.NetworksAnnotation]; found {
		t.Errorf("networks annotation = %q, want none", v)
	}
	if sts.Spec.Template.Spec.Affinity != nil {
		t.Errorf("affinity = %+v, want none", sts.Spec.Template.Spec.Affinity)
	}
	for _, arg := range runnerContainer(t, sts).Args {
		if arg == "--data-interface="+MDSDataInterface {
			t.Errorf("runner args = %v, want no --data-interface", runnerContainer(t, sts).Args)
		}
	}
}

// ── the NetworkAttachmentDefinition ─────────────────────────────────────────

var nadGVK = multus.DefinitionGVK

func dataNetworkScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := reconcilerScheme(t)
	scheme.AddKnownTypeWithName(nadGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(nadGVK.GroupVersion().WithKind(nadGVK.Kind+"List"), &unstructured.UnstructuredList{})
	return scheme
}

// dataNode is a node the metadata server can run on, attached to the data
// subnet through iface, or to no subnet when iface is empty.
func dataNode(t *testing.T, name, iface string) *corev1.Node {
	t.Helper()
	labels := map[string]string{KVMCapableLabel: KVMCapableValue, "pool": "kvm"}
	if iface != "" {
		labels[subnetKey(t)] = iface
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func ensureDataNetwork(
	t *testing.T, d *simplyblockv1alpha2.SimplyblockDriver, objs ...client.Object,
) (*MDSDataAttachment, client.Client, error) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(dataNetworkScheme(t)).WithObjects(append(objs, d)...).Build()
	got, err := EnsureMDSDataNetwork(context.Background(), c, d)
	return got, c, err
}

func nadConfig(t *testing.T, c client.Client, d *simplyblockv1alpha2.SimplyblockDriver) map[string]any {
	t.Helper()
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(nadGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: d.Namespace, Name: MDSDataNetworkName(d)}, nad); err != nil {
		t.Fatalf("reading the NetworkAttachmentDefinition: %v", err)
	}
	raw, _, _ := unstructured.NestedString(nad.Object, "spec", "config")
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("config %q: %v", raw, err)
	}
	if refs := nad.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != d.Name {
		t.Errorf("owner references = %+v, want the driver", refs)
	}
	return config
}

// The macvlan's parent is the interface most of the eligible nodes carry the
// subnet on, and the addresses come from the spec's range.
func TestEnsureMDSDataNetworkCreatesTheAttachmentOnTheCommonInterface(t *testing.T) {
	d := withDataNetwork(testDriver("simplyblock"))
	got, c, err := ensureDataNetwork(t, d,
		dataNode(t, "worker-1", testDataIface),
		dataNode(t, "worker-2", testDataIface),
		dataNode(t, "worker-3", "eth1"),
		// Not eligible: no KVM, so its interface is not counted.
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "master-1", Labels: map[string]string{subnetKey(t): "eth1"}}},
	)
	if err != nil {
		t.Fatalf("EnsureMDSDataNetwork: %v", err)
	}
	if got == nil || got.Interface != testDataIface || got.Network != MDSDataNetworkName(d) {
		t.Fatalf("attachment = %+v, want %s on ens16np0", got, MDSDataNetworkName(d))
	}
	config := nadConfig(t, c, d)
	if config["type"] != "macvlan" || config["master"] != testDataIface || config["mode"] != "bridge" {
		t.Errorf("config = %v, want a bridge-mode macvlan on ens16np0", config)
	}
	ipam, _ := config["ipam"].(map[string]any)
	if ipam["type"] != "whereabouts" || ipam["range"] != testDataSubnet ||
		ipam["range_start"] != "10.10.10.200" || ipam["range_end"] != "10.10.10.209" {
		t.Errorf("ipam = %v, want whereabouts over the spec's range", ipam)
	}
}

// Choosing again would restart the guest, so the recorded interface stays
// while a node still carries it, even when another one has become more common.
func TestEnsureMDSDataNetworkKeepsAnInterfaceThatStillWorks(t *testing.T) {
	d := withDataNetwork(testDriver("simplyblock"))
	_, c, err := ensureDataNetwork(t, d, dataNode(t, "worker-1", "eth1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*corev1.Node{dataNode(t, "worker-2", testDataIface), dataNode(t, "worker-3", testDataIface)} {
		if err := c.Create(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	got, err := EnsureMDSDataNetwork(context.Background(), c, d)
	if err != nil || got.Interface != "eth1" {
		t.Errorf("attachment = %+v, %v, want eth1 kept", got, err)
	}
}

// A recorded interface no eligible node carries any more would leave the pod
// unschedulable, so it is chosen again.
func TestEnsureMDSDataNetworkChoosesAgainWhenNoNodeCarriesTheInterface(t *testing.T) {
	d := withDataNetwork(testDriver("simplyblock"))
	_, c, err := ensureDataNetwork(t, d, dataNode(t, "worker-1", "eth1"))
	if err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "worker-1"}, node); err != nil {
		t.Fatal(err)
	}
	node.Labels[subnetKey(t)] = testDataIface
	if err := c.Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	got, err := EnsureMDSDataNetwork(context.Background(), c, d)
	if err != nil || got.Interface != testDataIface {
		t.Errorf("attachment = %+v, %v, want ens16np0", got, err)
	}
	if config := nadConfig(t, c, d); config["master"] != testDataIface {
		t.Errorf("master = %v, want ens16np0", config["master"])
	}
}

// A cluster where the data network cannot be attached still gets a metadata
// server, on the pod network, and the reason it is not attached.
func TestEnsureMDSDataNetworkFallsBackWhenNoEligibleNodeIsOnTheSubnet(t *testing.T) {
	d := withDataNetwork(testDriver("simplyblock"))
	got, _, err := ensureDataNetwork(t, d, dataNode(t, "worker-1", ""))
	var fallback *MDSDataNetworkFallback
	if got != nil || !errors.As(err, &fallback) || fallback.Reason != ReasonNoDataNetworkNode {
		t.Errorf("attachment = %+v, err = %v, want none and a %s fallback", got, err, ReasonNoDataNetworkNode)
	}
}

// Talos and K3s ship no Multus. A driver spec shared with an OpenShift cluster
// must not hold their metadata server back.
func TestEnsureMDSDataNetworkFallsBackWithoutMultus(t *testing.T) {
	d := withDataNetwork(testDriver("simplyblock"))
	c := fake.NewClientBuilder().WithScheme(dataNetworkScheme(t)).
		WithObjects(d, dataNode(t, "worker-1", testDataIface)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == nadGVK {
					return &meta.NoKindMatchError{GroupKind: nadGVK.GroupKind(), SearchedVersions: []string{"v1"}}
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	got, err := EnsureMDSDataNetwork(context.Background(), c, d)
	var fallback *MDSDataNetworkFallback
	if got != nil || !errors.As(err, &fallback) || fallback.Reason != ReasonMDSDataNetworkUnavailable {
		t.Errorf("attachment = %+v, err = %v, want none and a %s fallback", got, err, ReasonMDSDataNetworkUnavailable)
	}
}

func TestEnsureMDSDataNetworkIsNothingWithoutADataNetwork(t *testing.T) {
	got, _, err := ensureDataNetwork(t, withMDS(testDriver("simplyblock")), dataNode(t, "worker-1", testDataIface))
	if got != nil || err != nil {
		t.Errorf("attachment = %+v, %v, want none", got, err)
	}
}
