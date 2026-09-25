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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// Approval is one-way, and an approved document is frozen. Both rules exist to
// stop a document being edited out from under a deployment that is acting on it.
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

	edited := config.DeepCopy()
	edited.Spec.NodeSets[0].Groups[0].Workers = []string{"worker-1", "worker-2"}
	if err := apiClient.Update(context.Background(), edited); err == nil {
		t.Error("an approved document was edited")
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
