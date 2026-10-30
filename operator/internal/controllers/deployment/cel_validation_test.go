// Validation of the CEL rules compiled into the ClusterDeploymentConfig schema,
// run against a real apiserver.
//
// It lives here rather than under internal/webhook because there is no webhook
// involved: the rules are enforced by the apiserver itself, and envtest is the
// only place in the tree that starts one.
//
// The approval rules are the reason this file exists. They are the gate the
// whole deployment path runs through, they are expressed entirely in CEL, and
// they read a field whose presence a fake client cannot tell apart from its
// absence — so nothing short of an apiserver could have caught them being
// unsatisfiable.

package deployment

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aStoredDocument writes a document the way discovery writes one: approved
// unset, because nobody has reviewed it yet.
func aStoredDocument(t *testing.T, apiClient client.Client, name string) *simplyblockv1alpha2.ClusterDeploymentConfig {
	t.Helper()

	config := &simplyblockv1alpha2.ClusterDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
			Cluster: &simplyblockv1alpha2.ClusterTemplate{
				Name:              name + "-cluster",
				VCPUCount:         ptr.To(int32(4)),
				MaxSubsystemCount: ptr.To(int32(30)),
			},
			NodeSets: []simplyblockv1alpha2.NodeSet{{
				Name: "discovered",
				Groups: []simplyblockv1alpha2.NodeGroup{{
					Name:    "group-1",
					Workers: []string{"worker-1"},
					Devices: &simplyblockv1alpha2.DeviceSelection{
						NVMe: []string{"0000:00:02.0"},
					},
				}},
			}},
		},
	}
	if err := apiClient.Create(context.Background(), config); err != nil {
		t.Fatalf("storing the document: %v", err)
	}
	return config
}

// A document nobody has approved can be approved.
//
// This is the whole deployment path in one assertion. The approval rules guard
// an approved document against edits by reading oldSelf.approved, and a field
// omitted when false is a field the apiserver never stored, so that read found
// no key and failed the rule — which denied the very first approval and every
// one after it. Nothing could be deployed at all.
func TestAnUnapprovedDocumentCanBeApproved(t *testing.T) {
	apiClient := apiServer(t)
	config := aStoredDocument(t, apiClient, "approvable")

	config.Spec.Approved = true
	if err := apiClient.Update(context.Background(), config); err != nil {
		t.Fatalf("approving a document that nobody had approved: %v", err)
	}

	var fresh simplyblockv1alpha2.ClusterDeploymentConfig
	if err := apiClient.Get(context.Background(), client.ObjectKeyFromObject(config), &fresh); err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if !fresh.Spec.Approved {
		t.Error("the approval did not stick")
	}
}

// The document discovery writes says it is unapproved rather than leaving the
// question open.
//
// A reviewer reading a draft has to see the gate they are being asked to open,
// and a reader of the stored object has to find the field the rules read. Both
// are the same requirement: the key exists.
func TestAStoredDocumentCarriesItsApprovalFlag(t *testing.T) {
	apiClient := apiServer(t)
	config := aStoredDocument(t, apiClient, "explicit")

	stored := &simplyblockv1alpha2.ClusterDeploymentConfig{}
	if err := apiClient.Get(context.Background(), client.ObjectKeyFromObject(config), stored); err != nil {
		t.Fatalf("reading it back: %v", err)
	}

	// The typed read cannot tell an absent key from a false one, so the rules
	// are what the presence is asserted through: a rule reading the field on an
	// object that does not carry it fails rather than reading false.
	stored.Spec.Approved = true
	if err := apiClient.Update(context.Background(), stored); err != nil {
		t.Fatalf("the stored document carries no approval key: %v", err)
	}
}

// Approval is one-way. The freeze on the rest of an approved document is the
// admission webhook's (TestEditingAnApprovedDocumentIsRefused), because it
// makes one exception, the stretched role, which a schema rule cannot state.
func TestAnApprovedDocumentIsFrozen(t *testing.T) {
	apiClient := apiServer(t)
	config := aStoredDocument(t, apiClient, "frozen")

	config.Spec.Approved = true
	if err := apiClient.Update(context.Background(), config); err != nil {
		t.Fatalf("approving: %v", err)
	}

	withdrawn := config.DeepCopy()
	withdrawn.Spec.Approved = false
	err := apiClient.Update(context.Background(), withdrawn)
	if err == nil {
		t.Error("approval was withdrawn")
	} else if !strings.Contains(err.Error(), "withdraw") && !strings.Contains(err.Error(), "immutable") {
		t.Errorf("the refusal is %v, want one about withdrawal or immutability", err)
	}
}

// Regression: 2026-10-26-role-swap-never-delivered: the schema froze an approved
// document whole, so the hub's re-delivery of a site's document with its new
// stretched role was refused and no role swap reached a site. The apiserver
// admits the role change; the webhook admits it and nothing else.
func TestTheStretchedRoleOfAnApprovedDocumentCanChange(t *testing.T) {
	apiClient := apiServer(t)
	config := aStoredDocument(t, apiClient, "swappable")
	config.Spec.Cluster.Stretched = &simplyblockv1alpha2.StretchedSpec{
		Site: "a", PeerSite: "b", Role: simplyblockv1alpha2.StretchedRoleLeader,
	}
	config.Spec.Approved = true
	if err := apiClient.Update(context.Background(), config); err != nil {
		t.Fatalf("approving: %v", err)
	}
	swapped := config.DeepCopy()
	swapped.Spec.Cluster.Stretched.Role = simplyblockv1alpha2.StretchedRoleFollower
	if err := apiClient.Update(context.Background(), swapped); err != nil {
		t.Fatalf("the apiserver refused a role swap on an approved document: %v", err)
	}
}

// The freeze is on the document, not on the object.
//
// An approved document is one the expansion is acting on, so its spec is
// closed — but the controller still has to mark it and still has to report on
// it, and both of those live outside the spec. A rule written one level up
// would have frozen the object whole and left the controller unable to record
// what it did with it.
//
// This one was green when it was written: the rules are declared on the spec
// and always were. It is here because nothing else says so, and the next edit
// to those markers is one lifted pin away from locking the label the expansion
// selects on.
func TestAnApprovedDocumentStillTakesLabelsAndStatus(t *testing.T) {
	apiClient := apiServer(t)
	config := aStoredDocument(t, apiClient, "markable")

	config.Spec.Approved = true
	if err := apiClient.Update(context.Background(), config); err != nil {
		t.Fatalf("approving: %v", err)
	}

	labeled := config.DeepCopy()
	labeled.Labels = map[string]string{readyToDeploy: readyToDeployValue}
	if err := apiClient.Update(context.Background(), labeled); err != nil {
		t.Fatalf("the controller cannot mark an approved document: %v", err)
	}

	var fresh simplyblockv1alpha2.ClusterDeploymentConfig
	if err := apiClient.Get(context.Background(), client.ObjectKeyFromObject(config), &fresh); err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	fresh.Status.Phase = simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanding
	if err := apiClient.Status().Update(context.Background(), &fresh); err != nil {
		t.Fatalf("the controller cannot report on an approved document: %v", err)
	}
}

// The scheme rule reaches this kind too, because the document states the
// cluster's stripe with the cluster's own type. A rule declared on the shared
// type and reaching only one of the two schemas would leave the document — the
// thing a reviewer approves — the unguarded one.
func TestTheDocumentsSchemaRefusesAnUnsupportedScheme(t *testing.T) {
	apiClient := apiServer(t)

	config := &simplyblockv1alpha2.ClusterDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "stripe-", Namespace: "default"},
		Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
			Cluster: &simplyblockv1alpha2.ClusterTemplate{
				Name:              "a-cluster",
				VCPUCount:         ptr.To(int32(4)),
				MaxSubsystemCount: ptr.To(int32(30)),
				Stripe: &simplyblockv1alpha2.StripeSpec{
					DataChunks: ptr.To(int32(3)), ParityChunks: ptr.To(int32(1)),
				},
			},
			NodeSets: []simplyblockv1alpha2.NodeSet{{
				Name: "discovered",
				Groups: []simplyblockv1alpha2.NodeGroup{{
					Name: "group-1", Workers: []string{"worker-1"},
				}},
			}},
		},
	}

	err := apiClient.Create(context.Background(), config)
	if err == nil {
		t.Fatal("a document stating 3+1 was stored")
	}
	if !strings.Contains(err.Error(), "erasure-coding scheme") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// A group cannot ask for fewer journal managers than the control plane will accept:
// it requires at least three, and refuses a smaller count inside the node-add task,
// which runs after approval has made the document immutable.
//
// Regression: 2026-09-30-journal-count-below-three.
func TestAJournalCountBelowThreeIsRefusedBySchema(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name       string
		count      *int32
		wantDenied bool
	}{
		{name: "not stated"},
		{name: "three", count: ptr.To(int32(3))},
		{name: "five", count: ptr.To(int32(5))},
		{name: "two", count: ptr.To(int32(2)), wantDenied: true},
		{name: "one", count: ptr.To(int32(1)), wantDenied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := simplyblockv1alpha2.NodeGroup{Name: "group-1", Workers: []string{"worker-1"}}
			if tc.count != nil {
				group.JournalManager = &simplyblockv1alpha2.JournalManagerSpec{Count: tc.count}
			}
			config := &simplyblockv1alpha2.ClusterDeploymentConfig{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "journal-", Namespace: "default"},
				Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
					Cluster: &simplyblockv1alpha2.ClusterTemplate{
						Name:              "a-cluster",
						VCPUCount:         ptr.To(int32(4)),
						MaxSubsystemCount: ptr.To(int32(30)),
					},
					NodeSets: []simplyblockv1alpha2.NodeSet{{
						Name: "discovered", Groups: []simplyblockv1alpha2.NodeGroup{group},
					}},
				},
			}

			err := apiClient.Create(context.Background(), config)
			switch {
			case tc.wantDenied && err == nil:
				t.Fatal("the apiserver stored a journal count below three")
			case tc.wantDenied && !strings.Contains(err.Error(), "count"):
				t.Fatalf("refused for the wrong reason: %v", err)
			case !tc.wantDenied && err != nil:
				t.Fatalf("the apiserver refused a valid count: %v", err)
			}
		})
	}
}

// A document that names the ports block gets the control plane's own numbers
// filled in for whatever it leaves out, which is what makes the block worth
// naming: a reviewer sees the three a cluster will run with rather than the one
// somebody happened to state.
func TestThePortsBlockIsDefaultedByTheApiserver(t *testing.T) {
	apiClient := apiServer(t)
	config := aStoredDocument(t, apiClient, "ports-defaulted")
	config.Spec.Cluster.Ports = &simplyblockv1alpha2.ClusterPortsSpec{NVMf: ptr.To(int32(4430))}
	if err := apiClient.Update(context.Background(), config); err != nil {
		t.Fatalf("stating one port: %v", err)
	}

	stored := &simplyblockv1alpha2.ClusterDeploymentConfig{}
	if err := apiClient.Get(context.Background(), client.ObjectKeyFromObject(config), stored); err != nil {
		t.Fatalf("reading the document back: %v", err)
	}

	ports := stored.Spec.Cluster.Ports
	if ports == nil {
		t.Fatal("the stored document carries no ports block")
	}
	if got := ptr.IntFromOrZero(ports.NVMf); got != 4430 {
		t.Errorf("the NVMe-oF base is %d, want the stated 4430", got)
	}
	if got := ptr.IntFromOrZero(ports.Rpc); got != 8080 {
		t.Errorf("the RPC base is %d, want the control plane's 8080", got)
	}
	if got := ptr.IntFromOrZero(ports.NodeAgent); got != 50001 {
		t.Errorf("the node agent's port is %d, want the control plane's 50001", got)
	}
}

// The OpenShift block is what a deployment onto OpenShift states, so a document
// that names it without naming the distribution is a contradiction the
// apiserver refuses rather than the expansion ignoring it.
func TestAnOpenShiftBlockNeedsAnOpenShiftEnvironment(t *testing.T) {
	apiClient := apiServer(t)
	config := aStoredDocument(t, apiClient, "openshift-block")
	config.Spec.Cluster.OpenShift = &simplyblockv1alpha2.OpenShiftSpec{MachineConfigPool: "infra"}

	err := apiClient.Update(context.Background(), config)
	if err == nil {
		t.Fatal("a document with no environment carried an OpenShift block")
	}
	if !strings.Contains(err.Error(), "OpenShift") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}

	config.Spec.Environment = simplyblockv1alpha2.KubernetesEnvironmentOpenShift
	if err := apiClient.Update(context.Background(), config); err != nil {
		t.Fatalf("an OpenShift document was refused its own block: %v", err)
	}
}

// The kubelet and CPU topology flags are the environment's to decide everywhere
// but Vanilla, whose workers it does not describe, so a document states them
// only for a Vanilla fleet.
func TestTheKubeletAndTopologyFlagsNeedAVanillaEnvironment(t *testing.T) {
	apiClient := apiServer(t)
	for _, set := range []func(*simplyblockv1alpha2.ClusterTemplate){
		func(c *simplyblockv1alpha2.ClusterTemplate) { c.EnableKubeletConfiguration = ptr.To(false) },
		func(c *simplyblockv1alpha2.ClusterTemplate) { c.EnableCpuTopology = ptr.To(true) },
	} {
		config := aStoredDocument(t, apiClient, fmt.Sprintf("vanilla-flags-%d", time.Now().UnixNano()))
		set(config.Spec.Cluster)
		config.Spec.Environment = simplyblockv1alpha2.KubernetesEnvironmentTalos

		err := apiClient.Update(context.Background(), config)
		if err == nil {
			t.Fatal("a Talos document stated a flag Talos decides")
		}
		if !strings.Contains(err.Error(), "Vanilla") {
			t.Errorf("the refusal does not say what is wrong: %v", err)
		}

		config.Spec.Environment = simplyblockv1alpha2.KubernetesEnvironmentVanilla
		if err := apiClient.Update(context.Background(), config); err != nil {
			t.Fatalf("a Vanilla document was refused the flag: %v", err)
		}
	}
}

// A discovery run writes the managed service it detected into the document, so
// the schema has to take every one of them.
func TestTheSchemaTakesTheManagedServices(t *testing.T) {
	apiClient := apiServer(t)
	for _, environment := range []simplyblockv1alpha2.KubernetesEnvironment{
		simplyblockv1alpha2.KubernetesEnvironmentEKS,
		simplyblockv1alpha2.KubernetesEnvironmentGKE,
		simplyblockv1alpha2.KubernetesEnvironmentAKS,
		simplyblockv1alpha2.KubernetesEnvironmentOKE,
	} {
		config := aStoredDocument(t, apiClient, "environment-"+strings.ToLower(string(environment)))
		config.Spec.Environment = environment
		if err := apiClient.Update(context.Background(), config); err != nil {
			t.Errorf("the %s document was refused: %v", environment, err)
		}
	}
}

// The cluster template carries the cluster's arbitration settings, and the
// schema refuses them on a template that is not Dual, as it does on the
// StorageCluster they are copied to: the document is where the mistake can
// still be corrected.
func TestATemplateTakesArbitrationOnlyWhenDual(t *testing.T) {
	apiClient := apiServer(t)
	for _, tc := range []struct {
		name       string
		mode       simplyblockv1alpha2.ClusterMode
		enable     *bool
		settings   *simplyblockv1alpha2.ArbitrationSpec
		wantDenied bool
	}{
		{name: "dual with arbitration", mode: simplyblockv1alpha2.ClusterModeDual, enable: ptr.To(true),
			settings: &simplyblockv1alpha2.ArbitrationSpec{HoldMs: ptr.To(int32(2500))}},
		{name: "standard with arbitration", mode: simplyblockv1alpha2.ClusterModeStandard,
			enable: ptr.To(true), wantDenied: true},
		{name: "unstated mode with arbitration settings",
			settings: &simplyblockv1alpha2.ArbitrationSpec{HoldMs: ptr.To(int32(2500))}, wantDenied: true},
		{name: "single without arbitration", mode: simplyblockv1alpha2.ClusterModeSingle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := &simplyblockv1alpha2.ClusterDeploymentConfig{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "mode-", Namespace: "default"},
				Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
					Cluster: &simplyblockv1alpha2.ClusterTemplate{
						Name:              "mode-cluster",
						VCPUCount:         ptr.To(int32(4)),
						MaxSubsystemCount: ptr.To(int32(30)),
						ClusterMode:       tc.mode,
						EnableArbitration: tc.enable,
						Arbitration:       tc.settings,
					},
					NodeSets: []simplyblockv1alpha2.NodeSet{{
						Name: "discovered",
						Groups: []simplyblockv1alpha2.NodeGroup{{
							Name:    "group-1",
							Workers: []string{"worker-1"},
							Devices: &simplyblockv1alpha2.DeviceSelection{NVMe: []string{"0000:00:02.0"}},
						}},
					}},
				},
			}
			err := apiClient.Create(context.Background(), config)
			switch {
			case tc.wantDenied && err == nil:
				t.Fatal("the template was accepted, want the Dual-only refusal")
			case tc.wantDenied && !strings.Contains(err.Error(), "only valid when clusterMode is Dual"):
				t.Fatalf("refused for the wrong reason: %v", err)
			case !tc.wantDenied && err != nil:
				t.Fatalf("the template was refused: %v", err)
			}
			if err == nil {
				_ = apiClient.Delete(context.Background(), config)
			}
		})
	}
}

// Each node set of a supercluster's document becomes a member with a DaemonSet
// of its own, so the device class is one per node set rather than one per
// document (design-superclusters.md §5.1): two node sets may differ, and two
// groups of one node set may not.
func TestTheDeviceClassIsOnePerNodeSet(t *testing.T) {
	apiClient := apiServer(t)
	group := func(name, worker string, devices simplyblockv1alpha2.DeviceSelection) simplyblockv1alpha2.NodeGroup {
		return simplyblockv1alpha2.NodeGroup{Name: name, Workers: []string{worker}, Devices: &devices}
	}
	nvme := simplyblockv1alpha2.DeviceSelection{NVMe: []string{"0000:00:02.0"}}
	block := simplyblockv1alpha2.DeviceSelection{Block: []string{"/dev/sdb"}}

	config := aStoredDocument(t, apiClient, "per-set-class")
	config.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
		{Name: "fast", Groups: []simplyblockv1alpha2.NodeGroup{group("g", "worker-1", nvme)}},
		{Name: "dense", Groups: []simplyblockv1alpha2.NodeGroup{group("g", "worker-2", block)}},
	}
	if err := apiClient.Update(context.Background(), config); err != nil {
		t.Fatalf("two node sets of two device classes were refused: %v", err)
	}

	config.Spec.NodeSets[1].Groups = append(config.Spec.NodeSets[1].Groups, group("h", "worker-3", nvme))
	err := apiClient.Update(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), "device class") {
		t.Errorf("a node set mixing device classes was admitted, or refused for another reason: %v", err)
	}
}

// A node set overrides the cluster template only where the node set is a
// member of its own: a document of one node set creating one cluster states
// that cluster's sizing in the template (design-superclusters.md §5.8).
func TestANodeSetOverridesTheTemplateOnlyWhenItIsAMember(t *testing.T) {
	apiClient := apiServer(t)
	group := simplyblockv1alpha2.NodeGroup{
		Name: "g", Workers: []string{"worker-1"},
		Devices: &simplyblockv1alpha2.DeviceSelection{NVMe: []string{"0000:00:02.0"}},
	}
	overrides := &simplyblockv1alpha2.NodeSetClusterOverrides{VCPUCount: ptr.To(int32(8))}

	config := aStoredDocument(t, apiClient, "overrides")
	config.Spec.NodeSets[0].Cluster = overrides
	err := apiClient.Update(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), "overrides the cluster template") {
		t.Errorf("a single node set overriding the template was admitted, or refused for another reason: %v", err)
	}

	config = aStoredDocument(t, apiClient, "member-overrides")
	config.Spec.NodeSets = append(config.Spec.NodeSets,
		simplyblockv1alpha2.NodeSet{Name: "dense", Groups: []simplyblockv1alpha2.NodeGroup{group}, Cluster: overrides})
	if err := apiClient.Update(context.Background(), config); err != nil {
		t.Errorf("a member's node set overriding the template was refused: %v", err)
	}
}

// An override is held to the floor the template is: four vCPUs.
func TestANodeSetsVCPUOverrideIsHeldToTheTemplatesFloor(t *testing.T) {
	apiClient := apiServer(t)
	group := simplyblockv1alpha2.NodeGroup{
		Name: "g", Workers: []string{"worker-1"},
		Devices: &simplyblockv1alpha2.DeviceSelection{NVMe: []string{"0000:00:02.0"}},
	}
	for _, tc := range []struct {
		vcpu  int32
		admit bool
	}{{4, true}, {3, false}} {
		config := aStoredDocument(t, apiClient, fmt.Sprintf("vcpu-floor-%d", tc.vcpu))
		config.Spec.NodeSets = append(config.Spec.NodeSets, simplyblockv1alpha2.NodeSet{
			Name: "dense", Groups: []simplyblockv1alpha2.NodeGroup{group},
			Cluster: &simplyblockv1alpha2.NodeSetClusterOverrides{VCPUCount: ptr.To(tc.vcpu)},
		})
		err := apiClient.Update(context.Background(), config)
		if tc.admit && err != nil {
			t.Errorf("vcpuCount %d was refused: %v", tc.vcpu, err)
		}
		if !tc.admit && (err == nil || !strings.Contains(err.Error(), "vcpuCount")) {
			t.Errorf("vcpuCount %d was admitted, or refused for another reason: %v", tc.vcpu, err)
		}
	}
}

// withGroups replaces a stored document's node sets with the given groups,
// written unstructured so that the fields under test are stated the way a
// manifest states them.
func withGroups(t *testing.T, config *simplyblockv1alpha2.ClusterDeploymentConfig, groups ...map[string]any) *unstructured.Unstructured {
	t.Helper()
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(config)
	if err != nil {
		t.Fatal(err)
	}
	doc := &unstructured.Unstructured{Object: content}
	doc.SetGroupVersionKind(simplyblockv1alpha2.GroupVersion.WithKind("ClusterDeploymentConfig"))
	list := make([]any, 0, len(groups))
	for _, group := range groups {
		list = append(list, group)
	}
	if err := unstructured.SetNestedSlice(doc.Object, []any{map[string]any{"name": "discovered", "groups": list}},
		"spec", "nodeSets"); err != nil {
		t.Fatal(err)
	}
	return doc
}

// aGroup is a group of one worker with one NVMe device and the network fields
// given.
func aGroup(name, worker string, network map[string]any) map[string]any {
	group := map[string]any{
		"name":    name,
		"workers": []any{worker},
		"devices": map[string]any{"nvme": []any{"0000:00:02.0"}},
	}
	for key, value := range network {
		group[key] = value
	}
	return group
}

// A group names the NetworkAttachmentDefinitions its storage nodes are attached
// through, in place of host interfaces.
func TestAGroupNamesItsNetworkAttachments(t *testing.T) {
	apiClient := apiServer(t)
	config := aStoredDocument(t, apiClient, "attached")

	doc := withGroups(t, config, aGroup("g", "worker-1", map[string]any{
		"networkAttachment": map[string]any{"management": "storage-mgmt", "data": "storage-data"},
	}))
	if err := apiClient.Update(context.Background(), doc, client.FieldValidation("Strict")); err != nil {
		t.Fatalf("a group naming its network attachments was refused: %v", err)
	}
}

// Host networking and network attachments do not mix within a group.
func TestAGroupNamingInterfacesAndAttachmentsIsRefused(t *testing.T) {
	apiClient := apiServer(t)
	config := aStoredDocument(t, apiClient, "both-in-group")

	doc := withGroups(t, config, aGroup("g", "worker-1", map[string]any{
		"mgmtInterface":     "eth0",
		"networkAttachment": map[string]any{"management": "storage-mgmt"},
	}))
	err := apiClient.Update(context.Background(), doc, client.FieldValidation("Strict"))
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("a group naming an interface and an attachment was admitted, or refused for another reason: %v", err)
	}
}

// Nor within a document: every storage node of a deployment is on host
// networking, or every one is attached.
func TestADocumentMixingHostNetworkingAndAttachmentsIsRefused(t *testing.T) {
	apiClient := apiServer(t)
	config := aStoredDocument(t, apiClient, "mixed")

	doc := withGroups(t, config,
		aGroup("g", "worker-1", map[string]any{"mgmtInterface": "eth0"}),
		aGroup("h", "worker-2", map[string]any{"networkAttachment": map[string]any{"management": "storage-mgmt"}}),
	)
	err := apiClient.Update(context.Background(), doc, client.FieldValidation("Strict"))
	if err == nil || !strings.Contains(err.Error(), "host networking") {
		t.Errorf("a document mixing host networking and attachments was admitted, or refused for another reason: %v", err)
	}
}
