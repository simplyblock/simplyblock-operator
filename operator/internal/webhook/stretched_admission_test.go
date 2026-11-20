// Admission on one site of a stretched cluster: a follower runs no cluster-wide
// operation, and nobody runs an operation on the other site's remote nodes or
// their devices from this site.

package webhook

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	stretchedNS      = "simplyblock"
	stretchedCluster = "production"
)

func readerOf(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func siteCluster(role *simplyblockv1alpha2.StretchedRole) *simplyblockv1alpha2.StorageCluster {
	c := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: stretchedCluster, Namespace: stretchedNS},
	}
	if role != nil {
		c.Spec.EnableSyncReplication = ptr.To(true)
		c.Spec.Stretched = &simplyblockv1alpha2.StretchedSpec{Site: "b", PeerSite: "a", Role: *role}
	}
	return c
}

func createOf(t *testing.T, object any) admission.Request {
	t.Helper()
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    raw(t, object),
	}}
}

func clusterOpsFor(action simplyblockv1alpha2.StorageClusterOpsAction) *simplyblockv1alpha2.StorageClusterOps {
	return &simplyblockv1alpha2.StorageClusterOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops", Namespace: stretchedNS},
		Spec:       simplyblockv1alpha2.StorageClusterOpsSpec{ClusterRef: stretchedCluster, Action: action},
	}
}

var clusterWideActions = []simplyblockv1alpha2.StorageClusterOpsAction{
	simplyblockv1alpha2.StorageClusterOpsActionActivate,
	simplyblockv1alpha2.StorageClusterOpsActionExpand,
	simplyblockv1alpha2.StorageClusterOpsActionShutdown,
	simplyblockv1alpha2.StorageClusterOpsActionStart,
	simplyblockv1alpha2.StorageClusterOpsActionRestart,
	simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
}

// U-14: a follower's cluster-wide actions run on its leader, which owns them.
func TestAFollowerRefusesClusterWideOperations(t *testing.T) {
	follower := simplyblockv1alpha2.StretchedRoleFollower
	v := &StorageClusterOpsValidator{Client: readerOf(t, siteCluster(&follower))}

	for _, action := range clusterWideActions {
		t.Run(string(action), func(t *testing.T) {
			resp := v.Handle(context.Background(), createOf(t, clusterOpsFor(action)))
			if resp.Allowed {
				t.Fatalf("%s was admitted on a follower site", action)
			}
			if msg := resp.Result.Message; !strings.Contains(msg, "FollowerSite") || !strings.Contains(msg, "a") {
				t.Errorf("the refusal does not say FollowerSite and name the leader site: %s", msg)
			}
		})
	}
}

// A task cancel targets one control-plane task, which the backend owns, so a
// follower may still cancel one.
func TestAFollowerMayCancelATask(t *testing.T) {
	follower := simplyblockv1alpha2.StretchedRoleFollower
	v := &StorageClusterOpsValidator{Client: readerOf(t, siteCluster(&follower))}
	resp := v.Handle(context.Background(),
		createOf(t, clusterOpsFor(simplyblockv1alpha2.StorageClusterOpsActionCancelTask)))
	if !resp.Allowed {
		t.Fatalf("a task cancel was refused on a follower: %s", resp.Result.Message)
	}
}

// U-15 and U-17: the leader, and a standalone cluster, run every action.
func TestALeaderAndAStandaloneClusterAdmitEveryAction(t *testing.T) {
	leader := simplyblockv1alpha2.StretchedRoleLeader
	for name, cluster := range map[string]*simplyblockv1alpha2.StorageCluster{
		"leader":     siteCluster(&leader),
		"standalone": siteCluster(nil),
	} {
		v := &StorageClusterOpsValidator{Client: readerOf(t, cluster)}
		for _, action := range clusterWideActions {
			resp := v.Handle(context.Background(), createOf(t, clusterOpsFor(action)))
			if !resp.Allowed {
				t.Errorf("%s: %s was refused: %s", name, action, resp.Result.Message)
			}
		}
	}
}

func siteNode(name string, remote bool) *simplyblockv1alpha2.StorageNode {
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: stretchedNS},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: stretchedCluster,
			Config:     simplyblockv1alpha2.StorageNodeConfig{Site: "b"},
		},
	}
	if remote {
		node.Spec.Config.Site = "a"
		node.Spec.Stretched = &simplyblockv1alpha2.StretchedNodeSpec{Remote: true}
	} else {
		node.Spec.WorkerNode = "worker-1"
	}
	return node
}

func nodeOpsOn(node string) *simplyblockv1alpha2.StorageNodeOps {
	return &simplyblockv1alpha2.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: "node-ops", Namespace: stretchedNS},
		Spec: simplyblockv1alpha2.StorageNodeOpsSpec{
			NodeRef: node, Action: simplyblockv1alpha2.StorageNodeOpsActionRestart,
		},
	}
}

// U-86 and U-16: a remote node is its own site's to operate; this site's own
// nodes are this site's.
func TestAnOperationOnARemoteNodeIsRefused(t *testing.T) {
	v := &StorageNodeOpsValidator{Client: readerOf(t, siteNode("remote-1", true), siteNode("local-1", false))}

	resp := v.Handle(context.Background(), createOf(t, nodeOpsOn("remote-1")))
	if resp.Allowed {
		t.Fatal("an operation on a remote node was admitted")
	}
	if msg := resp.Result.Message; !strings.Contains(msg, "RemoteNode") || !strings.Contains(msg, "site a") {
		t.Errorf("the refusal does not say RemoteNode and name the owning site: %s", msg)
	}

	if resp := v.Handle(context.Background(), createOf(t, nodeOpsOn("local-1"))); !resp.Allowed {
		t.Errorf("an operation on this site's own node was refused: %s", resp.Result.Message)
	}
}

func siteDevice(name, node string) *simplyblockv1alpha2.StorageDevice {
	return &simplyblockv1alpha2.StorageDevice{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: stretchedNS},
		Spec:       simplyblockv1alpha2.StorageDeviceSpec{NodeRef: node, DeviceID: name},
	}
}

func deviceOpsOn(device string) *simplyblockv1alpha2.StorageDeviceOps {
	return &simplyblockv1alpha2.StorageDeviceOps{
		ObjectMeta: metav1.ObjectMeta{Name: "device-ops", Namespace: stretchedNS},
		Spec:       simplyblockv1alpha2.StorageDeviceOpsSpec{DeviceRef: device},
	}
}

// U-91: a remote node's devices are listed for visibility, and operated on by
// their own site.
func TestAnOperationOnARemoteNodesDeviceIsRefused(t *testing.T) {
	v := &StorageDeviceOpsValidator{Client: readerOf(t,
		siteNode("remote-1", true), siteNode("local-1", false),
		siteDevice("remote-1-dev", "remote-1"), siteDevice("local-1-dev", "local-1"))}

	resp := v.Handle(context.Background(), createOf(t, deviceOpsOn("remote-1-dev")))
	if resp.Allowed {
		t.Fatal("an operation on a remote node's device was admitted")
	}
	if msg := resp.Result.Message; !strings.Contains(msg, "RemoteNode") || !strings.Contains(msg, "site a") {
		t.Errorf("the refusal does not say RemoteNode and name the owning site: %s", msg)
	}

	if resp := v.Handle(context.Background(), createOf(t, deviceOpsOn("local-1-dev"))); !resp.Allowed {
		t.Errorf("an operation on this site's own device was refused: %s", resp.Result.Message)
	}
}
