// Operations on a stretched cluster's remote nodes and their devices.
//
// A remote node is the peer site's node, listed here for visibility and managed
// by its own site's operator. Admission refuses an operation against one; these
// cases cover the controller's own refusal, for an operation admitted while the
// webhook was down or on a failed read.

package node

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// remotePeerNodeName is the peer site's node as this site lists it.
const remotePeerNodeName = "peer-node"

// aRemoteNode is the peer site's node: no worker here, the peer's site, and the
// backend identity the control plane reports.
func aRemoteNode() *simplyblockv1alpha2.StorageNode {
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: remotePeerNodeName, Namespace: opsNamespace},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: opsCluster,
			Stretched:  &simplyblockv1alpha2.StretchedNodeSpec{Remote: true},
		},
	}
	node.Spec.Config.Site = "site-a"
	node.Status.UUID = opsPeerID
	return node
}

// Regression: 2026-10-22-remote-node-ops-reach-backend.
// A StorageNodeOps against the peer site's remote node took the node's lock and
// called the control plane on it, operating on a node this site does not manage.
func TestAnOperationOnARemoteNodeFailsWithoutABackendCall(t *testing.T) {
	ops := anOperation("a-suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend)
	ops.Spec.NodeRef = remotePeerNodeName
	api := aControlPlane()
	r, apiClient := anOpsWorld(t, api, aRemoteNode(), ops)

	for range 4 {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: opsNamespace, Name: "a-suspend"},
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	got := operationRead(t, apiClient, "a-suspend")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed for an operation on a remote node", got.Status.Phase)
	}
	if !strings.HasPrefix(got.Status.Message, "RemoteNode:") ||
		!strings.Contains(got.Status.Message, "site-a") {
		t.Errorf("message = %q, want a RemoteNode refusal naming site-a", got.Status.Message)
	}
	if len(api.calls) != 0 {
		t.Errorf("the control plane was called for a remote node: %v", api.calls)
	}
}

// Regression: 2026-10-22-remote-device-ops-reach-backend.
// A StorageDeviceOps against a device of the peer site's remote node restarted
// the device through the control plane, operating on a device this site does
// not manage.
func TestAnOperationOnARemoteNodesDeviceFailsWithoutABackendCall(t *testing.T) {
	device := deviceObject()
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: device.Spec.NodeRef, Namespace: deviceOpsNamespace},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			Stretched: &simplyblockv1alpha2.StretchedNodeSpec{Remote: true},
		},
	}
	node.Spec.Config.Site = "site-a"
	api := &deviceCalls{status: cpDeviceOnline}
	w := newDeviceWorld(t, api, deviceOperation(), device, node)

	for range 4 {
		if _, err := w.r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: deviceOpsNamespace, Name: deviceOpsName},
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	var got simplyblockv1alpha2.StorageDeviceOps
	if err := w.c.Get(context.Background(),
		types.NamespacedName{Namespace: deviceOpsNamespace, Name: deviceOpsName}, &got); err != nil {
		t.Fatalf("read the operation: %v", err)
	}
	if got.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed for a device of a remote node", got.Status.Phase)
	}
	if !strings.HasPrefix(got.Status.Message, "RemoteNode:") ||
		!strings.Contains(got.Status.Message, "site-a") {
		t.Errorf("message = %q, want a RemoteNode refusal naming site-a", got.Status.Message)
	}
	if len(api.issued) != 0 || api.restarts != 0 {
		t.Errorf("the control plane was asked %v for a remote node's device", api.issued)
	}
}
