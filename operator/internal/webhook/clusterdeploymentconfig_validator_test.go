// Tests for the approval guard.
//
// The asymmetry is the design (design-clusterdeploymentconfig.md §5.1): a draft
// is admitted on its structure alone, however wrong it is about the world, and
// the edit that sets spec.approved is answered against the live cluster. What
// separates the two is that §3.2 makes an approved document immutable, so a
// document admitted with a mistake in it cannot be corrected, only deleted.

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The namespace every document and cluster below lives in, and the cluster they
// name. What the cases differ in is what is there and what the document says
// about it, never what anything is called.
const (
	testDeploymentNamespace = "simplyblock"
	testDeploymentCluster   = "production"
	testDeploymentConfig    = "rack-one"
)

func testWorker(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// testDeploymentStorageCluster is the cluster every test in this file names,
// because the checks under test are about whether a cluster of that name exists
// and what class it is, never about which name it carries. It carries the 1+0 a
// cluster of one node can carry, so that a case about something else is not also
// a case about erasure coding; a case about the stripe states its own.
func testDeploymentStorageCluster(
	class simplyblockv1alpha2.StorageClusterDeviceClass,
) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testDeploymentCluster,
			Namespace: testDeploymentNamespace,
		},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			DeviceClass: class,
			Stripe: &simplyblockv1alpha2.StripeSpec{
				DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(0)),
			},
		},
	}
}

// testSecondCluster is the cluster a second document names, for the checks about
// two documents rather than about one document and the world.
const testSecondCluster = "rack-two"

// testConfig is a document that passes every check: one group, one worker that
// exists, NVMe devices, and a cluster of its own to create.
//
// It states 1+0 because one worker carries no other scheme: every redundant one
// needs at least three storage nodes, and a document saying nothing about
// erasure coding means the control plane's 1+1.
func testConfig() *simplyblockv1alpha2.ClusterDeploymentConfig {
	return &simplyblockv1alpha2.ClusterDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testDeploymentConfig,
			Namespace: testDeploymentNamespace,
		},
		Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
			Cluster: &simplyblockv1alpha2.ClusterTemplate{
				Name:              testDeploymentCluster,
				MaxSubsystemCount: ptr.To(int32(10)),
				VCPUCount:         ptr.To(int32(4)),
				Stripe: &simplyblockv1alpha2.StripeSpec{
					DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(0)),
				},
			},
			NodeSets: []simplyblockv1alpha2.NodeSet{{
				Name: "rack-b",
				Groups: []simplyblockv1alpha2.NodeGroup{{
					Name:          "workers",
					Workers:       []string{"worker-1"},
					MgmtInterface: "eth0",
					Devices: &simplyblockv1alpha2.DeviceSelection{
						NVMe: []string{"0000:5e:00.0"},
					},
				}},
			}},
		},
	}
}

// approved is the document the approving edit produces.
func approved(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) *simplyblockv1alpha2.ClusterDeploymentConfig {
	config.Spec.Approved = true
	return config
}

// grows re-points a document at a cluster that already exists, which is the
// growth document of §6.
func grows(
	config *simplyblockv1alpha2.ClusterDeploymentConfig, cluster string,
) *simplyblockv1alpha2.ClusterDeploymentConfig {
	config.Spec.ClusterRef = cluster
	config.Spec.Cluster = nil
	return config
}

// withBlockDevices swaps the document's device class, which is what a growth
// document naming the wrong one for its cluster looks like.
func withBlockDevices(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) *simplyblockv1alpha2.ClusterDeploymentConfig {
	config.Spec.NodeSets[0].Groups[0].Devices = &simplyblockv1alpha2.DeviceSelection{
		Block: []string{"/dev/sdb"},
	}
	return config
}

// testStorageNode is a node the cluster a growth document names already has.
func testStorageNode(name, worker string) *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testDeploymentNamespace},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: testDeploymentCluster,
			WorkerNode: worker,
			Slot:       ptr.To(int32(0)),
		},
	}
}

func configRaw(t *testing.T, config *simplyblockv1alpha2.ClusterDeploymentConfig) runtime.RawExtension {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal ClusterDeploymentConfig: %v", err)
	}
	return runtime.RawExtension{Raw: raw}
}

// review runs one admission request against a validator holding the objects the
// cluster is said to carry.
func review(
	t *testing.T,
	existing []client.Object,
	operation admissionv1.Operation,
	old, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) admission.Response {
	t.Helper()

	scheme := newPoolScheme(t)
	validator := &ClusterDeploymentConfigValidator{
		Client:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing...).Build(),
		Decoder: admission.NewDecoder(scheme),
	}

	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: operation,
		Namespace: testDeploymentNamespace,
		Object:    configRaw(t, config),
	}}
	if old != nil {
		request.OldObject = configRaw(t, old)
	}
	return validator.Handle(context.Background(), request)
}

// approve is the edit the whole webhook is about: a draft that exists, updated
// to set spec.approved.
func approve(
	t *testing.T, existing []client.Object, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) admission.Response {
	t.Helper()

	draft := config.DeepCopy()
	draft.Spec.Approved = false
	return review(t, existing, admissionv1.Update, draft, approved(config))
}

func mustAllow(t *testing.T, response admission.Response) {
	t.Helper()
	if !response.Allowed {
		t.Fatalf("the document was refused: %s", response.Result.Message)
	}
}

func mustDeny(t *testing.T, response admission.Response, mentions ...string) {
	t.Helper()
	if response.Allowed {
		t.Fatal("the document was admitted, and it should not have been")
	}
	for _, want := range mentions {
		if !strings.Contains(response.Result.Message, want) {
			t.Errorf("the refusal %q does not mention %q", response.Result.Message, want)
		}
	}
}

// A draft may name a worker that does not exist and a cluster that does not
// exist, because a document that could not be saved until it was correct is a
// document nobody can work on (§5.1). The controller reports what it found in
// status.message and the reviewer fixes it in place.
func TestADraftIsAdmittedOnItsStructureAlone(t *testing.T) {
	draft := grows(testConfig(), "no-such-cluster")
	draft.Spec.NodeSets[0].Groups[0].Workers = []string{"no-such-worker"}

	mustAllow(t, review(t, nil, admissionv1.Create, nil, draft))
	mustAllow(t, review(t, nil, admissionv1.Update, draft.DeepCopy(), draft))
}

// The approving edit is answered against the live cluster, and a document whose
// four answers are all yes is admitted.
func TestApprovingADocumentThatChecksOutIsAdmitted(t *testing.T) {
	mustAllow(t, approve(t, []client.Object{testWorker("worker-1")}, testConfig()))
}

// A document may also be written already approved, which is the same edit
// arriving as a create.
func TestACreateThatIsAlreadyApprovedIsValidatedToo(t *testing.T) {
	mustAllow(t, review(t, []client.Object{testWorker("worker-1")},
		admissionv1.Create, nil, approved(testConfig())))

	mustDeny(t, review(t, nil, admissionv1.Create, nil, approved(testConfig())),
		"worker-1")
}

// Every worker named by every group has to exist as a Node (§5.1).
func TestApprovingIsRefusedWhenAWorkerIsNotANode(t *testing.T) {
	config := testConfig()
	config.Spec.NodeSets[0].Groups[0].Workers = []string{"worker-1", "worker-2"}

	response := approve(t, []client.Object{testWorker("worker-1")}, config)
	mustDeny(t, response, "worker-2")
	if strings.Contains(response.Result.Message, "worker-1") {
		t.Errorf("the refusal %q names a worker that does exist", response.Result.Message)
	}
}

// spec.clusterRef has to resolve to a StorageCluster when it is set (§6's
// ClusterNotFound row).
func TestApprovingIsRefusedWhenClusterRefResolvesToNothing(t *testing.T) {
	mustDeny(t, approve(t, []client.Object{testWorker("worker-1")},
		grows(testConfig(), "no-such-cluster")), "no-such-cluster")
}

// ...and to nothing when it is not (§6's ClusterExists row). The refusal says
// what to do instead, because the document that was wanted is a growth document.
func TestApprovingIsRefusedWhenTheClusterAlreadyExists(t *testing.T) {
	existing := []client.Object{
		testWorker("worker-1"),
		testDeploymentStorageCluster(""),
	}

	mustDeny(t, approve(t, existing, testConfig()),
		testDeploymentCluster, "spec.clusterRef")
}

// A document that names neither a cluster to create nor one to grow describes no
// deployment at all, and the expansion refuses it with ClusterNotFound — after
// the document has become immutable.
func TestApprovingIsRefusedWhenTheDocumentNamesNoCluster(t *testing.T) {
	config := testConfig()
	config.Spec.Cluster = nil

	mustDeny(t, approve(t, []client.Object{testWorker("worker-1")}, config),
		"spec.cluster")
}

// The class the groups name has to match the cluster's, where one is named. The
// nodes would otherwise be rejected one at a time by StorageNodeValidator, which
// is a slower way to learn it and leaves a half-expanded deployment behind
// (§5.1).
func TestApprovingAGrowthDocumentOfTheWrongDeviceClassIsRefused(t *testing.T) {
	existing := []client.Object{
		testWorker("worker-1"),
		testDeploymentStorageCluster(
			simplyblockv1alpha2.StorageClusterDeviceClassNVMe),
	}

	mustDeny(t, approve(t, existing, withBlockDevices(grows(testConfig(), testDeploymentCluster))),
		"LogicalBlock", "NVMe")
}

func TestApprovingAGrowthDocumentOfTheRightDeviceClassIsAdmitted(t *testing.T) {
	existing := []client.Object{
		testWorker("worker-1"),
		testDeploymentStorageCluster(
			simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock),
	}

	mustAllow(t, approve(t, existing,
		withBlockDevices(grows(testConfig(), testDeploymentCluster))))
}

// A cluster carrying no class is an NVMe cluster, which is what the field
// defaults to and what describes every cluster predating it.
func TestAGrowthDocumentReadsAnUnstatedClassAsNVMe(t *testing.T) {
	existing := []client.Object{
		testWorker("worker-1"),
		testDeploymentStorageCluster(""),
	}

	mustAllow(t, approve(t, existing, grows(testConfig(), testDeploymentCluster)))
	mustDeny(t, approve(t, existing, withBlockDevices(grows(testConfig(), testDeploymentCluster))),
		"NVMe")
}

// No other approved config may already own the cluster this one would create
// (§5.1). Both would race to create it, and the loser is an immutable Failed
// document reporting a deployment that never happened.
func TestApprovingASecondCreateOfTheSameClusterIsRefused(t *testing.T) {
	other := approved(testConfig())
	other.Name = testSecondCluster

	mustDeny(t, approve(t, []client.Object{testWorker("worker-1"), other}, testConfig()),
		testSecondCluster)
}

// An unapproved document naming the same cluster is not an owner. It is a draft,
// and whichever of the two is approved first becomes the owner.
func TestADraftNamingTheSameClusterDoesNotBlockAnApproval(t *testing.T) {
	other := testConfig()
	other.Name = testSecondCluster

	mustAllow(t, approve(t, []client.Object{testWorker("worker-1"), other}, testConfig()))
}

// Growth is a second document, so two approved documents adding nodes to one
// cluster is the design rather than a conflict (§6).
func TestASecondGrowthDocumentIsAdmitted(t *testing.T) {
	other := approved(grows(testConfig(), testDeploymentCluster))
	other.Name = testSecondCluster

	existing := []client.Object{
		testWorker("worker-1"),
		testDeploymentStorageCluster(""),
		other,
	}

	mustAllow(t, approve(t, existing, grows(testConfig(), testDeploymentCluster)))
}

// The document being approved is not another config that owns its cluster.
func TestADocumentDoesNotConflictWithItself(t *testing.T) {
	existing := []client.Object{testWorker("worker-1"), approved(testConfig())}

	mustAllow(t, approve(t, existing, testConfig()))
}

// §3.2's rules restated: the webhook names the field that was edited and what to
// do instead, where CEL names the rule that failed (§5.2).
func TestEditingAnApprovedDocumentIsRefused(t *testing.T) {
	old := approved(testConfig())
	edited := approved(testConfig())
	edited.Spec.NodeSets[0].Groups[0].Workers = []string{"worker-2"}

	response := review(t, []client.Object{testWorker("worker-1"), testWorker("worker-2")},
		admissionv1.Update, old, edited)
	mustDeny(t, response, "spec.clusterRef")
}

func TestWithdrawingApprovalIsRefused(t *testing.T) {
	old := approved(testConfig())
	withdrawn := testConfig()

	mustDeny(t, review(t, []client.Object{testWorker("worker-1")},
		admissionv1.Update, old, withdrawn), "approved")
}

// The operator labels an approved config so that a selector can find one (§5),
// which is an edit to a document whose spec is immutable.
func TestLabelingAnApprovedDocumentIsAdmitted(t *testing.T) {
	old := approved(testConfig())
	labeled := approved(testConfig())
	labeled.Labels = map[string]string{"storage.simplyblock.io/ready-to-deploy": "true"}

	mustAllow(t, review(t, nil, admissionv1.Update, old, labeled))
}

// A reviewer fixing an immutable document wants the whole list, not one problem
// per apply.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	config := testConfig()
	config.Spec.NodeSets[0].Groups[0].Workers = []string{"no-such-worker"}

	mustDeny(t, approve(t, nil, grows(config, "no-such-cluster")),
		"no-such-worker", "no-such-cluster")
}

// A delete is not the validator's business: the document is a record, and
// deleting it is how a wrong one is disposed of.
func TestDeletingADocumentIsNotTheValidatorsBusiness(t *testing.T) {
	mustAllow(t, review(t, nil, admissionv1.Delete, nil, approved(testConfig())))
}

// A namespaced object created through a namespaced endpoint may arrive with the
// field unset, because the path carries it instead.
func TestTheRequestNamespaceIsUsedWhenTheObjectCarriesNone(t *testing.T) {
	config := approved(testConfig())
	config.Namespace = ""

	mustDeny(t, review(t, nil, admissionv1.Create, nil, config), "worker-1")
}

// A document whose fleet is too small for its erasure coding is refused at the
// approving edit, which is the last moment it can be corrected: the control
// plane validates the scheme on the cluster create and counts devices at
// activation, never nodes, so nothing after this edit refuses it.
func TestApprovingADocumentTooSmallForItsStripeIsRefused(t *testing.T) {
	config := testConfig()
	config.Spec.Cluster.Stripe = &simplyblockv1alpha2.StripeSpec{
		DataChunks: ptr.To(int32(2)), ParityChunks: ptr.To(int32(1)),
	}

	mustDeny(t, approve(t, []client.Object{testWorker("worker-1")}, config),
		"2+1", "4")
}

// A scheme the control plane's supported set does not hold is refused here too,
// because the create that refuses it runs after approval has made the document
// immutable.
func TestApprovingAnUnsupportedSchemeIsRefused(t *testing.T) {
	config := testConfig()
	config.Spec.Cluster.Stripe = &simplyblockv1alpha2.StripeSpec{
		DataChunks: ptr.To(int32(3)), ParityChunks: ptr.To(int32(1)),
	}

	mustDeny(t, approve(t, []client.Object{testWorker("worker-1")}, config), "3+1")
}

// A growth document is answered against the cluster it grows, so the nodes that
// cluster already has count toward the minimum and the approval stands.
func TestApprovingAGrowthDocumentCountsTheClustersNodes(t *testing.T) {
	config := grows(testConfig(), testDeploymentCluster)
	config.Spec.NodeSets[0].Groups[0].Workers = []string{"worker-3"}

	cluster := testDeploymentStorageCluster(simplyblockv1alpha2.StorageClusterDeviceClassNVMe)
	cluster.Spec.Stripe = &simplyblockv1alpha2.StripeSpec{
		DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(1)),
	}

	mustAllow(t, approve(t, []client.Object{
		testWorker("worker-3"), cluster,
		testStorageNode("node-1", "worker-1"), testStorageNode("node-2", "worker-2"),
	}, config))
}
