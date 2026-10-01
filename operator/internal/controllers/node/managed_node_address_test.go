// Whether NodeAddress gives the control plane something it can actually
// reach: the per-pod Service DNS name a control plane on this Kubernetes
// cluster resolves itself, or the worker's own real address when the control
// plane runs on a different one entirely and could never resolve that DNS.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func managedControlPlaneSingleton() *simplyblockv1alpha2.ControlPlane {
	return &simplyblockv1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: SingletonControlPlaneName, Namespace: opsNamespace},
		Spec: simplyblockv1alpha2.ControlPlaneSpec{
			Source: simplyblockv1alpha2.ControlPlaneSource{
				Managed: &simplyblockv1alpha2.ManagedControlPlane{
					Endpoint: "https://hub.example.com:5000",
				},
			},
		},
	}
}

func localControlPlaneSingleton() *simplyblockv1alpha2.ControlPlane {
	return &simplyblockv1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: SingletonControlPlaneName, Namespace: opsNamespace},
		Spec: simplyblockv1alpha2.ControlPlaneSpec{
			Source: simplyblockv1alpha2.ControlPlaneSource{
				Local: &simplyblockv1alpha2.LocalControlPlane{Image: "docker.io/simplyblock/simplyblock:1.0"},
			},
		},
	}
}

func workerNode(name, internalIP string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: internalIP},
			},
		},
	}
}

// A managed control plane runs on a Kubernetes cluster that can never resolve
// this cluster's own Service DNS, so it is given the worker's real address --
// one the storage-node-api pod already answers on directly, since it runs
// with hostNetwork.
func TestNodeAddressUsesTheWorkersRealIPWhenTheControlPlaneIsManaged(t *testing.T) {
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(managedControlPlaneSingleton(), workerNode(opsWorker, "192.168.10.112")).
		Build()
	w := &Workload{Client: c}

	got := w.NodeAddress(context.Background(), opsWorker, opsNamespace)

	if want := "192.168.10.112:5000"; got != want {
		t.Errorf("NodeAddress = %q, want %q", got, want)
	}
}

// A local control plane resolves the per-pod DNS name itself, and this is the
// existing, well-tested precondition for that: nothing about a same-cluster
// deployment changes.
func TestNodeAddressKeepsTheDNSNameForALocalControlPlane(t *testing.T) {
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(localControlPlaneSingleton(), workerNode(opsWorker, "192.168.10.112")).
		Build()
	w := &Workload{Client: c}

	got := w.NodeAddress(context.Background(), opsWorker, opsNamespace)

	if want := utils.StorageNodeSetAPIAddress(opsWorker, opsNamespace); got != want {
		t.Errorf("NodeAddress = %q, want the per-pod DNS name %q", got, want)
	}
}

// No ControlPlane singleton at all -- every fixture in this package before
// this file -- falls back to exactly the address it always resolved to. This
// is what keeps the fix additive.
func TestNodeAddressFallsBackToDNSWhenTheControlPlaneCannotBeRead(t *testing.T) {
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(workerNode(opsWorker, "192.168.10.112")).
		Build()
	w := &Workload{Client: c}

	got := w.NodeAddress(context.Background(), opsWorker, opsNamespace)

	if want := utils.StorageNodeSetAPIAddress(opsWorker, opsNamespace); got != want {
		t.Errorf("NodeAddress = %q, want the per-pod DNS name %q", got, want)
	}
}

// A managed control plane but a worker Node this reader cannot find (a stale
// cache, a name that does not match) falls back the same way, rather than
// handing the control plane an empty or malformed address.
func TestNodeAddressFallsBackToDNSWhenTheWorkerNodeCannotBeRead(t *testing.T) {
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(managedControlPlaneSingleton()).
		Build()
	w := &Workload{Client: c}

	got := w.NodeAddress(context.Background(), opsWorker, opsNamespace)

	if want := utils.StorageNodeSetAPIAddress(opsWorker, opsNamespace); got != want {
		t.Errorf("NodeAddress = %q, want the per-pod DNS name %q", got, want)
	}
}
