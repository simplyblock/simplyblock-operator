// The gates a node passes before it is added, and what the add carries.
//
// Adding a backend node is not idempotent and the call adds every socket of a
// worker at once, so the machine's job is to be sure exactly once that the add is
// both possible and necessary. Two of its steps divert to adoption for that
// reason: a deployment being taken over wholesale, and a backend node already at
// the worker's address, which is what a POST whose response was lost looks like
// from here.
//
// The parameters are the node describing itself. Every value but the subsystem
// cap comes from its own spec.config, which is what stopped a fleet default being
// the source of truth and the node a cache of it.
//
// design-storagenode.md §4.2 and §4.3.

package node

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// anUnprovisionedNode is a node with no backend behind it yet, at the step being
// exercised.
func anUnprovisionedNode(at nodeStep) *simplyblockv1alpha2.StorageNode {
	node := anOpsNode()
	node.Status.UUID = ""
	node.Status.Step.State = string(at)
	return node
}

// A cluster that requires a fault group holds a node that declares none, rather
// than rejecting it: the value can arrive later, and holding is what makes
// filling it in sufficient.
func TestANodeWithNoFaultGroupIsHeldRatherThanRefused(t *testing.T) {
	cluster := anOpsCluster()
	cluster.Spec.EnableFailureDomains = ptr.To(true)
	r, _ := aSteadyNode(t, aControlPlane())

	next, done, err := r.checkConfig(context.Background(),
		anUnprovisionedNode(stepCheckingConfig), cluster)

	var blocked *blockedStepError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want the node held for want of a fault group", err)
	}
	if blocked.reason != FailureDomainMissing {
		t.Errorf("the hold is announced as %q, want %q", blocked.reason, FailureDomainMissing)
	}
	if done || next != stepCheckingConfig {
		t.Errorf("next, done = %s, %v; a held node stays where it is", next, done)
	}
}

// A node that declares one passes, and so does every node of a cluster that does
// not ask for fault groups at all.
func TestANodeThatDeclaresItsFaultGroupPasses(t *testing.T) {
	cluster := anOpsCluster()
	cluster.Spec.EnableFailureDomains = ptr.To(true)
	node := anUnprovisionedNode(stepCheckingConfig)
	node.Spec.Config.FailureDomain = "rack-1"
	r, _ := aSteadyNode(t, aControlPlane())

	next, done, err := r.checkConfig(context.Background(), node, cluster)
	if err != nil {
		t.Fatalf("checking the configuration: %v", err)
	}
	if !done || next != stepAwaitingSlot {
		t.Errorf("next, done = %s, %v; want the node moving on to wait for a slot", next, done)
	}

	indifferent, done, err := r.checkConfig(context.Background(),
		anUnprovisionedNode(stepCheckingConfig), anOpsCluster())
	if err != nil {
		t.Fatalf("checking the configuration: %v", err)
	}
	if !done || indifferent != stepAwaitingSlot {
		t.Errorf("a cluster that asks for no fault groups held a node that declares none")
	}
}

// A backend node already at the worker's address is adopted rather than added.
// That covers a POST whose response was lost after the control plane committed,
// as well as a node this operator never added.
func TestANodeAlreadyAtTheWorkersAddressIsAdopted(t *testing.T) {
	api := aControlPlane()
	r, _ := aSteadyNode(t, api, aWorker(opsWorker, true))

	next, done, err := r.checkConfig(context.Background(),
		anUnprovisionedNode(stepCheckingConfig), anOpsCluster())
	if err != nil {
		t.Fatalf("checking the configuration: %v", err)
	}
	if !done || next != stepAdopting {
		t.Errorf("next, done = %s, %v; want the node diverted to adoption", next, done)
	}
}

// A deployment being taken over wholesale diverts before the host check, because
// an adopted node is already running and its API answering is not this
// operator's precondition to establish.
func TestAnUpgradeAdoptionDivertsBeforeTheHostIsProbed(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "simplyblock-" + opsCluster + "-upgrade",
		Namespace: opsNamespace,
	}}
	r, _ := aSteadyNode(t, aControlPlane(), secret)

	next, done, err := r.checkHost(context.Background(),
		anUnprovisionedNode(stepCheckingHost), anOpsCluster())
	if err != nil {
		t.Fatalf("checking the host: %v", err)
	}
	if !done || next != stepAdopting {
		t.Errorf("next, done = %s, %v; want the node diverted to adoption", next, done)
	}
}

// The add carries what the node says about itself, and the cluster contributes
// only what belongs to the fleet.
func TestTheAddCarriesWhatTheNodeSaysAboutItself(t *testing.T) {
	node := anUnprovisionedNode(stepPosting)
	node.Spec.Config.SpdkImage = "example.test/spdk:v1"
	node.Spec.Config.SpdkProxyImage = "example.test/spdk-proxy:v1"
	node.Spec.Config.SpdkSystemMemory = "4g"
	node.Spec.Config.JournalManager = &simplyblockv1alpha2.JournalManagerSpec{
		PercentPerDevice: ptr.To(int32(5)), Count: ptr.To(int32(2)),
	}

	cluster := anOpsCluster()
	cluster.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{
		MgmtInterface:  "eth0",
		DataInterfaces: []string{"eth1"},
		EnableFormat4K: ptr.To(true),
	}

	r, _ := aSteadyNode(t, aControlPlane())
	params := r.addParams(node, cluster)

	if params.SPDKImage != "example.test/spdk:v1" ||
		params.SPDKProxyImage != "example.test/spdk-proxy:v1" ||
		params.SpdkSystemMemory != "4g" {
		t.Errorf("params = %+v, want the images and memory the node declares", params)
	}
	if params.JMPercent != 5 || params.HaJMCount != 2 {
		t.Errorf("journal = %d%% over %d, want what the node declares",
			params.JMPercent, params.HaJMCount)
	}
	if params.InterfaceName != "eth0" || len(params.DataNics) != 1 {
		t.Errorf("params = %+v, want the interfaces the fleet declares", params)
	}
	if !params.Format4K {
		t.Error("the fleet asked for 4K formatting and the add does not carry it")
	}
	if params.CRName != opsCluster || params.CRNameSpace != opsNamespace ||
		params.CRPlural != "storageclusters" {
		t.Errorf("params = %+v, want the cluster object the node belongs to", params)
	}
	if params.NodeAddress == "" {
		t.Error("the add names no address, which is what the control plane resolves the node by")
	}
}

// A node that declares no journal share is added with the default the control
// plane was always given. The count beside it is not defaulted here any more.
//
// It used to be, as the three this case asserted, and three is the control
// plane's answer for one parity chunk with failure domains off rather than an
// answer for every cluster. Sending it made a 2+2 deployment refuse every node
// add. The count is now left out of the request, which is what makes the control
// plane compute the one its own rule requires.
func TestAnUnstatedJournalShareIsTheDefaultAndTheCountIsNot(t *testing.T) {
	r, _ := aSteadyNode(t, aControlPlane())

	params := r.addParams(anUnprovisionedNode(stepPosting), anOpsCluster())

	if params.JMPercent != 3 {
		t.Errorf("the journal share is %d%%, want the default 3", params.JMPercent)
	}
	if params.HaJMCount != 0 {
		t.Errorf("the add states ha_jm_count=%d for a node that states none, "+
			"which answers a question the control plane answers", params.HaJMCount)
	}
}

// The two vocabularies differ: this API names a fault group after the rack or
// the power feed somebody would say out loud, and the control plane indexes one.
// A label seeded from an index sends its number, and a name is left to the
// control plane to assign.
func TestOnlyAFaultGroupThatIsANumberIsSentToTheControlPlane(t *testing.T) {
	r, _ := aSteadyNode(t, aControlPlane())

	numbered := anUnprovisionedNode(stepPosting)
	numbered.Spec.Config.FailureDomain = "2"
	if index := r.addParams(numbered, anOpsCluster()).FailureDomain; index == nil || *index != 2 {
		t.Errorf("failureDomain = %v, want the index the label spells", index)
	}

	named := anUnprovisionedNode(stepPosting)
	named.Spec.Config.FailureDomain = "rack-1"
	if index := r.addParams(named, anOpsCluster()).FailureDomain; index != nil {
		t.Errorf("failureDomain = %v, want none: the control plane has no field for a name",
			index)
	}

	if index := r.addParams(anUnprovisionedNode(stepPosting), anOpsCluster()).FailureDomain; index != nil {
		t.Errorf("failureDomain = %v, want none for a node that declares no group", index)
	}
}

// Posting is the one call the machine makes, and the claim that guards it was
// made by the transition into the step rather than by anything here.
func TestPostingIssuesTheAddOnce(t *testing.T) {
	api := aControlPlane()
	r, _ := aSteadyNode(t, api)

	next, done, err := r.performNodeStep(context.Background(),
		anUnprovisionedNode(stepPosting), anOpsCluster(), stepPosting)
	if err != nil {
		t.Fatalf("posting: %v", err)
	}
	if !done || next != stepResolving {
		t.Errorf("next, done = %s, %v; want the node waiting for the UUID the add produces",
			next, done)
	}
	if asked := api.asked("AddNode"); asked != 1 {
		t.Errorf("AddNode was issued %d time(s), want once", asked)
	}
}

// A step no provisioning path declares is a downgrade or a hand-edited object,
// and reconciling again does not resolve either.
func TestAStepNoProvisioningPathDeclaresIsRefused(t *testing.T) {
	r, _ := aSteadyNode(t, aControlPlane())

	_, _, err := r.performNodeStep(context.Background(),
		anUnprovisionedNode(stepPosting), anOpsCluster(), nodeStep("Nowhere"))

	if err == nil {
		t.Error("a step that belongs to no provisioning path was accepted")
	}
}

// A cluster the control plane has not created yet is nothing to add a node to,
// so the node holds and says which of the two it is waiting for.
func TestANodeWaitsForItsClusterToExistInTheControlPlane(t *testing.T) {
	r, apiClient := aSteadyNode(t, aControlPlane())

	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsCluster}
	if err := apiClient.Get(context.Background(), key, &cluster); err != nil {
		t.Fatalf("reading the cluster: %v", err)
	}
	cluster.Status.UUID = ""
	if err := apiClient.Status().Update(context.Background(), &cluster); err != nil {
		t.Fatalf("clearing the cluster's UUID: %v", err)
	}
	node := nodeRead(t, apiClient)
	node.Status.UUID = ""
	if err := apiClient.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("clearing the node's UUID: %v", err)
	}

	settle(t, r)

	if message := nodeRead(t, apiClient).Status.Message; message == "" {
		t.Error("the node says nothing about what it is waiting for")
	}
}
