// Whether the cluster serves NetworkAttachmentDefinitions, which ones a
// namespace holds, and what each one's CNI configuration says about the host.
//
// The configurations below are the shapes Multus is actually given: a single
// plugin, a plugin list, a host-device by name and by PCI address, and a
// definition with no configuration at all, which Multus resolves from a file on
// each node. The two on the lab-talos cluster are among them verbatim.

package inventory

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestParseNetworkAttachmentConfigReadsAnIPVLANMaster(t *testing.T) {
	got := ParseNetworkAttachmentConfig(`{
	  "cniVersion": "0.3.1",
	  "name": "storage-ipvlan",
	  "type": "ipvlan",
	  "master": "enp1s0",
	  "mode": "l2",
	  "capabilities": { "ips": true },
	  "ipam": { "type": "static" }
	}`)
	want := NetworkAttachmentConfig{Type: "ipvlan", HostInterface: "enp1s0", Mode: "l2", IPAM: "static"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseNetworkAttachmentConfigReadsAHostDeviceByName(t *testing.T) {
	got := ParseNetworkAttachmentConfig(`{
	  "cniVersion": "0.3.1",
	  "name": "storage-vf",
	  "type": "host-device",
	  "device": "enp2s0",
	  "ipam": { "type": "static" }
	}`)
	want := NetworkAttachmentConfig{Type: "host-device", HostInterface: "enp2s0", IPAM: "static"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A host-device named by slot names no interface, and the slot is what a reader
// matches against the worker's NICs instead.
func TestParseNetworkAttachmentConfigReadsAHostDeviceBySlot(t *testing.T) {
	got := ParseNetworkAttachmentConfig(`{"type": "host-device", "pciBusID": "0000:3b:00.1", "ipam": {"type": "whereabouts"}}`)
	want := NetworkAttachmentConfig{Type: "host-device", PCIAddress: "0000:3b:00.1", IPAM: "whereabouts"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A plugin list chains plugins, and the first is the one that creates the
// interface: tuning, bandwidth, and portmap after it change one that exists.
func TestParseNetworkAttachmentConfigReadsTheFirstPluginOfAList(t *testing.T) {
	got := ParseNetworkAttachmentConfig(`{
	  "cniVersion": "0.4.0",
	  "name": "storage",
	  "plugins": [
	    {"type": "macvlan", "master": "bond0.120", "mode": "bridge", "ipam": {"type": "whereabouts"}},
	    {"type": "tuning", "mtu": 9000}
	  ]
	}`)
	want := NetworkAttachmentConfig{Type: "macvlan", HostInterface: "bond0.120", Mode: "bridge", IPAM: "whereabouts"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseNetworkAttachmentConfigReadsABridge(t *testing.T) {
	got := ParseNetworkAttachmentConfig(`{"type": "bridge", "bridge": "br-storage", "ipam": {"type": "host-local"}}`)
	want := NetworkAttachmentConfig{Type: "bridge", HostInterface: "br-storage", IPAM: "host-local"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// An empty configuration is legal: Multus then reads the one named after the
// definition from each node's CNI directory, which is not something the API
// can be asked about.
func TestParseNetworkAttachmentConfigSaysWhyAnEmptyConfigNamesNothing(t *testing.T) {
	got := ParseNetworkAttachmentConfig("")
	if got.Type != "" || got.HostInterface != "" {
		t.Errorf("an empty config was read as %+v", got)
	}
	if got.Problem == "" {
		t.Error("an empty config carries no problem, so a reader cannot tell it from a plugin naming no interface")
	}
}

func TestParseNetworkAttachmentConfigReportsAConfigThatDoesNotParse(t *testing.T) {
	got := ParseNetworkAttachmentConfig(`{"type": "ipvlan", "master":`)
	if got.Problem == "" {
		t.Errorf("a truncated config was read as %+v with no problem", got)
	}
}

// nadGVR is what the fake dynamic client lists.
var nadGVR = schema.GroupVersionResource{Group: "k8s.cni.cncf.io", Version: "v1", Resource: "network-attachment-definitions"}

func nadObject(namespace, name, config string, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("k8s.cni.cncf.io/v1")
	obj.SetKind("NetworkAttachmentDefinition")
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetAnnotations(annotations)
	if config != "" {
		_ = unstructured.SetNestedField(obj.Object, config, "spec", "config")
	}
	return obj
}

// dynamicWith files the objects under the kind's real resource. Handed to the
// constructor, they would be filed under the resource the fake guesses from the
// kind, which drops the hyphens and lists nothing.
func dynamicWith(objects ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nadGVR: "NetworkAttachmentDefinitionList"})
	for _, obj := range objects {
		if err := client.Tracker().Create(nadGVR, obj, obj.GetNamespace()); err != nil {
			panic(err)
		}
	}
	return client
}

func discoveryServing(served bool) *discoveryfake.FakeDiscovery {
	fake := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{}}
	if served {
		fake.Resources = []*metav1.APIResourceList{{
			GroupVersion: "k8s.cni.cncf.io/v1",
			APIResources: []metav1.APIResource{{Name: "network-attachment-definitions", Namespaced: true, Kind: "NetworkAttachmentDefinition"}},
		}}
	}
	return fake
}

func TestCollectNetworkAttachmentsReadsTheNamespacesDefinitionsInNameOrder(t *testing.T) {
	source := NetworkAttachmentSource{
		Discovery: discoveryServing(true),
		Dynamic: dynamicWith(
			nadObject("simplyblock", "storage-vf", `{"type": "host-device", "device": "enp2s0", "ipam": {"type": "static"}}`, nil),
			nadObject("simplyblock", "storage-ipvlan", `{"type": "ipvlan", "master": "enp1s0", "mode": "l2", "ipam": {"type": "static"}}`, nil),
			nadObject("elsewhere", "other", `{"type": "macvlan", "master": "eth9"}`, nil),
		),
		Namespace: "simplyblock",
	}

	got := CollectNetworkAttachments(context.Background(), source)

	if got.State != NetworkAttachmentsServed {
		t.Fatalf("state is %q (%s), want %q", got.State, got.Detail, NetworkAttachmentsServed)
	}
	if len(got.Definitions) != 2 {
		t.Fatalf("got %d definitions, want the namespace's 2: %+v", len(got.Definitions), got.Definitions)
	}
	first, second := got.Definitions[0], got.Definitions[1]
	if first.Name != "storage-ipvlan" || first.Namespace != "simplyblock" || first.Config.HostInterface != "enp1s0" {
		t.Errorf("first definition is %+v", first)
	}
	if second.Name != "storage-vf" || second.Config.Type != "host-device" || second.Config.HostInterface != "enp2s0" {
		t.Errorf("second definition is %+v", second)
	}
}

// The SR-IOV device plugin's resource is on an annotation rather than in the
// configuration, and it is what the scheduler places a pod by.
func TestCollectNetworkAttachmentsCarriesTheDevicePluginResource(t *testing.T) {
	source := NetworkAttachmentSource{
		Discovery: discoveryServing(true),
		Dynamic: dynamicWith(nadObject("simplyblock", "storage-sriov", `{"type": "sriov", "ipam": {"type": "static"}}`,
			map[string]string{"k8s.v1.cni.cncf.io/resourceName": "intel.com/sriov_netdevice"})),
		Namespace: "simplyblock",
	}

	got := CollectNetworkAttachments(context.Background(), source)
	if len(got.Definitions) != 1 || got.Definitions[0].ResourceName != "intel.com/sriov_netdevice" {
		t.Errorf("got %+v, want the device plugin resource carried", got.Definitions)
	}
}

// A cluster without Multus does not serve the kind at all. That is an answer,
// and the one that keeps a run on host networking.
func TestCollectNetworkAttachmentsReportsAKindTheClusterDoesNotServe(t *testing.T) {
	source := NetworkAttachmentSource{Discovery: discoveryServing(false), Dynamic: dynamicWith(), Namespace: "simplyblock"}

	got := CollectNetworkAttachments(context.Background(), source)
	if got.State != NetworkAttachmentsNotServed {
		t.Errorf("state is %q (%s), want %q", got.State, got.Detail, NetworkAttachmentsNotServed)
	}
}

// Without a discovery client the list itself says so: the API server answers
// 404 for a resource it does not serve.
func TestCollectNetworkAttachmentsReadsANotFoundListAsNotServed(t *testing.T) {
	dyn := dynamicWith()
	dyn.PrependReactor("list", "network-attachment-definitions", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(nadGVR.GroupResource(), "")
	})

	got := CollectNetworkAttachments(context.Background(), NetworkAttachmentSource{Dynamic: dyn, Namespace: "simplyblock"})
	if got.State != NetworkAttachmentsNotServed {
		t.Errorf("state is %q (%s), want %q", got.State, got.Detail, NetworkAttachmentsNotServed)
	}
}

// A list that was refused is not a cluster without definitions, and reading it
// as one would put a fleet that has Multus on host networking without a word.
func TestCollectNetworkAttachmentsReportsARefusedListAsUnread(t *testing.T) {
	dyn := dynamicWith()
	dyn.PrependReactor("list", "network-attachment-definitions", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(nadGVR.GroupResource(), "", errors.New("no RBAC"))
	})

	got := CollectNetworkAttachments(context.Background(),
		NetworkAttachmentSource{Discovery: discoveryServing(true), Dynamic: dyn, Namespace: "simplyblock"})
	if got.State != NetworkAttachmentsUnread {
		t.Errorf("state is %q, want %q", got.State, NetworkAttachmentsUnread)
	}
	if got.Detail == "" {
		t.Error("an unread list carries no reason")
	}
}

// A collection with no cluster source leaves the reading untaken, which is not
// the same as a cluster without the kind.
func TestCollectLeavesNetworkAttachmentsUntakenWithoutASource(t *testing.T) {
	inv, _ := Collect(context.Background(), Config{
		SysfsRoot: t.TempDir(), ProcRoot: t.TempDir(), DevRoot: t.TempDir(), HostRoot: t.TempDir(),
	})
	if inv.NetworkAttachments.State != "" {
		t.Errorf("state is %q with no source given", inv.NetworkAttachments.State)
	}
}

func TestCollectReadsNetworkAttachmentsFromTheGivenSource(t *testing.T) {
	inv, _ := Collect(context.Background(), Config{
		SysfsRoot: t.TempDir(), ProcRoot: t.TempDir(), DevRoot: t.TempDir(), HostRoot: t.TempDir(),
		Kubernetes: KubernetesSources{NetworkAttachments: NetworkAttachmentSource{
			Discovery: discoveryServing(true),
			Dynamic:   dynamicWith(nadObject("simplyblock", "storage-ipvlan", `{"type": "ipvlan", "master": "enp1s0"}`, nil)),
			Namespace: "simplyblock",
		}},
	})
	if inv.NetworkAttachments.State != NetworkAttachmentsServed || len(inv.NetworkAttachments.Definitions) != 1 {
		t.Errorf("got %+v", inv.NetworkAttachments)
	}
	if inv.Environment.Distribution != "" {
		t.Errorf("the environment was concluded (%q) from a source given only for the attachments", inv.Environment.Distribution)
	}
}
