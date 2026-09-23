// What the StorageNode guard admits and refuses.
//
// The two halves are tested separately because they answer different questions.
// The update half is about who is writing, and needs no cluster; the create half
// is about what the node says against the cluster it names, and is decided
// entirely by reading that object.

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const operatorNS = "simplyblock-operator-system"

// operatorUser and someoneElse are the two identities every case here is one of.
const (
	operatorUser = "system:serviceaccount:" + operatorNS + ":simplyblock-operator"
	someoneElse  = "kubernetes-admin"
)

// testNode is a node that agrees with testStorageCluster in every respect, so a
// case states only what it is about.
func testNode(mutate func(*simplyblockv1alpha2.StorageNode)) *simplyblockv1alpha2.StorageNode {
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-1", Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: "production",
			WorkerNode: "worker-2",
			Config: simplyblockv1alpha2.StorageNodeConfig{
				Sizing: simplyblockv1alpha2.StorageNodeSizing{
					VCPUCount:        ptr.To(int32(8)),
					MinHugePagesSize: "100G",
				},
			},
		},
	}
	if mutate != nil {
		mutate(node)
	}
	return node
}

// testStorageCluster is an NVMe cluster sized at eight vCPUs.
func nodeTestCluster(
	mutate func(*simplyblockv1alpha2.StorageCluster),
) *simplyblockv1alpha2.StorageCluster {
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			VCPUCount:        ptr.To(int32(8)),
			MinHugePagesSize: "100G",
			DeviceClass:      simplyblockv1alpha2.StorageClusterDeviceClassNVMe,
		},
	}
	if mutate != nil {
		mutate(cluster)
	}
	return cluster
}

func validatorFor(t *testing.T, objects ...client.Object) *StorageNodeValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	return &StorageNodeValidator{
		Client:            fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		OperatorNamespace: operatorNS,
	}
}

func raw(t *testing.T, object any) runtime.RawExtension {
	t.Helper()
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return runtime.RawExtension{Raw: encoded}
}

func createReq(t *testing.T, node *simplyblockv1alpha2.StorageNode, user string) admission.Request {
	t.Helper()
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    raw(t, node),
		UserInfo:  authenticationv1.UserInfo{Username: user},
	}}
}

func updateReq(
	t *testing.T, oldNode, newNode *simplyblockv1alpha2.StorageNode, user string,
) admission.Request {
	t.Helper()
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		Object:    raw(t, newNode),
		OldObject: raw(t, oldNode),
		UserInfo:  authenticationv1.UserInfo{Username: user},
	}}
}

// The three fields of §3.2 have exactly one legitimate writer. A marker would lock
// the operator out along with everyone else, so this webhook is their only guard.
func TestTheOperatorOnlyFieldsAreRefusedToEveryoneElse(t *testing.T) {
	validator := validatorFor(t, nodeTestCluster(nil))

	for _, tc := range []struct {
		field  string
		mutate func(*simplyblockv1alpha2.StorageNode)
	}{
		{"spec.workerNode", func(n *simplyblockv1alpha2.StorageNode) {
			n.Spec.WorkerNode = "worker-4"
		}},
		{"spec.config.pcieAllowList", func(n *simplyblockv1alpha2.StorageNode) {
			n.Spec.Config.PcieAllowList = []string{"0000:5e:00.0"}
		}},
		{"spec.config.sizing", func(n *simplyblockv1alpha2.StorageNode) {
			n.Spec.Config.Sizing.VCPUCount = ptr.To(int32(16))
		}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			before, after := testNode(nil), testNode(tc.mutate)

			refused := validator.Handle(context.Background(), updateReq(t, before, after, someoneElse))
			if refused.Allowed {
				t.Errorf("%s was admitted from a user and only the operator may write it", tc.field)
			}
			if !strings.Contains(refused.Result.Message, tc.field) {
				t.Errorf("the refusal does not name %s: %s", tc.field, refused.Result.Message)
			}

			admitted := validator.Handle(context.Background(), updateReq(t, before, after, operatorUser))
			if !admitted.Allowed {
				t.Errorf("%s was refused to the operator, which is its one writer: %s",
					tc.field, admitted.Result.Message)
			}
		})
	}
}

// An update that touches none of the three is nobody's business but the writer's.
func TestAnUpdateTouchingNoGuardedFieldIsAdmitted(t *testing.T) {
	validator := validatorFor(t, nodeTestCluster(nil))
	before := testNode(nil)
	after := testNode(func(n *simplyblockv1alpha2.StorageNode) {
		n.Spec.Config.SpdkSystemMemory = "8G"
	})

	response := validator.Handle(context.Background(), updateReq(t, before, after, someoneElse))
	if !response.Allowed {
		t.Errorf("a mutable field was refused: %s", response.Result.Message)
	}
}

// spec.clusterRef is immutable from creation, so a node naming a cluster that is
// not there can never be corrected. Refusing the create asks for the delete-and-
// rewrite that is the only remedy anyway.
func TestANodeNamingNoClusterIsRefused(t *testing.T) {
	validator := validatorFor(t)

	response := validator.Handle(context.Background(), createReq(t, testNode(nil), someoneElse))
	if response.Allowed {
		t.Fatal("a node naming a cluster that does not exist was admitted")
	}
	if !strings.Contains(response.Result.Message, "does not exist") {
		t.Errorf("the refusal does not say the cluster is missing: %s", response.Result.Message)
	}
}

// A cluster is built out of one class of backend storage, because an
// erasure-coding stripe placed across both is written and rebuilt at the slower
// one's rate.
func TestDeviceNamesMustBeOfTheClusterSClass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		class   simplyblockv1alpha2.StorageClusterDeviceClass
		devices []string
		refused string
	}{
		{
			name:    "a path on an NVMe cluster",
			class:   simplyblockv1alpha2.StorageClusterDeviceClassNVMe,
			devices: []string{"/dev/sdb"},
			refused: "named by PCI address",
		},
		{
			name:    "a PCI address on a logical-block cluster",
			class:   simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock,
			devices: []string{"0000:5e:00.0"},
			refused: "named by path",
		},
		{
			name:    "both at once",
			class:   simplyblockv1alpha2.StorageClusterDeviceClassNVMe,
			devices: []string{"0000:5e:00.0", "/dev/sdb"},
			refused: "mixes PCI addresses",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validator := validatorFor(t, nodeTestCluster(
				func(c *simplyblockv1alpha2.StorageCluster) { c.Spec.DeviceClass = tc.class }))
			node := testNode(func(n *simplyblockv1alpha2.StorageNode) {
				n.Spec.Config.DeviceNames = tc.devices
			})

			response := validator.Handle(context.Background(), createReq(t, node, someoneElse))
			if response.Allowed {
				t.Fatalf("%v was admitted on a %s cluster", tc.devices, tc.class)
			}
			if !strings.Contains(response.Result.Message, tc.refused) {
				t.Errorf("the refusal does not explain the class: %s", response.Result.Message)
			}
		})
	}
}

// Each class admits its own spelling, which is the other half of the rule above.
func TestDeviceNamesOfTheClusterSClassAreAdmitted(t *testing.T) {
	for class, devices := range map[simplyblockv1alpha2.StorageClusterDeviceClass][]string{
		simplyblockv1alpha2.StorageClusterDeviceClassNVMe:         {"0000:5e:00.0", "0000:5f:00.0"},
		simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock: {"/dev/sdb", "nvme0n1"},
	} {
		validator := validatorFor(t, nodeTestCluster(
			func(c *simplyblockv1alpha2.StorageCluster) { c.Spec.DeviceClass = class }))
		node := testNode(func(n *simplyblockv1alpha2.StorageNode) {
			n.Spec.Config.DeviceNames = devices
		})

		response := validator.Handle(context.Background(), createReq(t, node, someoneElse))
		if !response.Allowed {
			t.Errorf("%v was refused on a %s cluster: %s", devices, class, response.Result.Message)
		}
	}
}

// The PCI filters match on something a logical block device does not have, so they
// are refused rather than ignored: a filter that silently selects nothing is a node
// that comes up with no devices and no reason given.
func TestThePCIFiltersAreRefusedOnALogicalBlockCluster(t *testing.T) {
	validator := validatorFor(t, nodeTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.DeviceClass = simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock
	}))

	for field, mutate := range map[string]func(*simplyblockv1alpha2.StorageNode){
		"spec.config.pcieAllowList": func(n *simplyblockv1alpha2.StorageNode) {
			n.Spec.Config.PcieAllowList = []string{"0000:5e:00.0"}
		},
		"spec.config.pcieDenyList": func(n *simplyblockv1alpha2.StorageNode) {
			n.Spec.Config.PcieDenyList = []string{"0000:5e:00.0"}
		},
		"spec.config.pcieModel": func(n *simplyblockv1alpha2.StorageNode) {
			n.Spec.Config.PcieModel = "Samsung"
		},
	} {
		response := validator.Handle(context.Background(),
			createReq(t, testNode(mutate), someoneElse))
		if response.Allowed {
			t.Errorf("%s was admitted on a logical-block cluster", field)
			continue
		}
		if !strings.Contains(response.Result.Message, field) {
			t.Errorf("the refusal does not name %s: %s", field, response.Result.Message)
		}
	}
}

// A node's sizing is a stamp of what its cluster was built with. A fleet whose
// nodes differ is a fleet mid-roll, never a fleet somebody described that way.
func TestAUserSNodeMustAgreeWithTheFleetSSizing(t *testing.T) {
	validator := validatorFor(t, nodeTestCluster(nil))

	for _, tc := range []struct {
		name   string
		mutate func(*simplyblockv1alpha2.StorageNode)
	}{
		{"a different core count", func(n *simplyblockv1alpha2.StorageNode) {
			n.Spec.Config.Sizing.VCPUCount = ptr.To(int32(16))
		}},
		{"no core count at all", func(n *simplyblockv1alpha2.StorageNode) {
			n.Spec.Config.Sizing.VCPUCount = nil
		}},
		{"a different huge-page floor", func(n *simplyblockv1alpha2.StorageNode) {
			n.Spec.Config.Sizing.MinHugePagesSize = "1T"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := validator.Handle(context.Background(),
				createReq(t, testNode(tc.mutate), someoneElse))
			if response.Allowed {
				t.Errorf("%s was admitted from a user", tc.name)
			}
		})
	}
}

// The operator writes a node's sizing itself, and a rolling hardware upgrade is
// the case where it deliberately differs. A model that refused that would force
// the whole fleet to be re-sized at once or not at all.
func TestTheOperatorMayCreateANodeSizedAgainstTheFleet(t *testing.T) {
	validator := validatorFor(t, nodeTestCluster(nil))
	node := testNode(func(n *simplyblockv1alpha2.StorageNode) {
		n.Spec.Config.Sizing.VCPUCount = ptr.To(int32(16))
	})

	response := validator.Handle(context.Background(), createReq(t, node, operatorUser))
	if !response.Allowed {
		t.Errorf("a mid-roll node was refused to the operator: %s", response.Result.Message)
	}
}

// An unstated class is NVMe, which is what the CRD defaults it to and what
// describes every cluster that predates the field.
func TestAClusterWithNoStatedClassIsNVMe(t *testing.T) {
	validator := validatorFor(t, nodeTestCluster(
		func(c *simplyblockv1alpha2.StorageCluster) { c.Spec.DeviceClass = "" }))
	node := testNode(func(n *simplyblockv1alpha2.StorageNode) {
		n.Spec.Config.DeviceNames = []string{"/dev/sdb"}
	})

	response := validator.Handle(context.Background(), createReq(t, node, someoneElse))
	if response.Allowed {
		t.Error("a device path was admitted on a cluster that states no class, which is NVMe")
	}
}
