// What a StorageCluster's storage-node DaemonSet defaults its image to when
// spec.storageNodes.image is unset.
//
// A local control plane's own image doubles as the default, since a
// self-hosted deployment's control plane and its storage nodes are one
// release. A managed control plane is a different Kubernetes cluster's
// install and says nothing about this one's storage nodes, so
// ManagedControlPlane.StorageNodeImage is the only source of a default there
// -- without it, the workload can never be built at all.

package node

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

func aStorageCluster() *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: opsCluster, Namespace: opsNamespace},
	}
}

// An explicit spec.storageNodes.image always wins, whatever the ControlPlane
// says.
func TestImagePrefersTheClustersOwnSpec(t *testing.T) {
	scheme := testsupport.NewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(localControlPlaneSingleton()).
		Build()
	r := &StorageNodeWorkloadReconciler{Client: c, Namespace: opsNamespace}

	cluster := aStorageCluster()
	cluster.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{
		Image: "docker.io/simplyblock/simplyblock:from-the-spec",
	}

	got, err := r.image(context.Background(), cluster)
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	if want := "docker.io/simplyblock/simplyblock:from-the-spec"; got != want {
		t.Errorf("image = %q, want %q", got, want)
	}
}

// A local control plane's own image is the default, exactly as it always was.
func TestImageDefaultsToTheLocalControlPlanesImage(t *testing.T) {
	scheme := testsupport.NewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(localControlPlaneSingleton()).
		Build()
	r := &StorageNodeWorkloadReconciler{Client: c, Namespace: opsNamespace}

	got, err := r.image(context.Background(), aStorageCluster())
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	if want := "docker.io/simplyblock/simplyblock:1.0"; got != want {
		t.Errorf("image = %q, want the local control plane's own %q", got, want)
	}
}

// A managed control plane names a storage-node image of its own, since its
// own spec.source.managed carries no image at all -- it is a different
// Kubernetes cluster's install.
func TestImageDefaultsToTheManagedControlPlanesStorageNodeImage(t *testing.T) {
	cp := managedControlPlaneSingleton()
	cp.Spec.Source.Managed.StorageNodeImage = "docker.io/simplyblock/simplyblock:from-managed"

	scheme := testsupport.NewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &StorageNodeWorkloadReconciler{Client: c, Namespace: opsNamespace}

	got, err := r.image(context.Background(), aStorageCluster())
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	if want := "docker.io/simplyblock/simplyblock:from-managed"; got != want {
		t.Errorf("image = %q, want the managed control plane's storage-node image %q", got, want)
	}
}

// A managed control plane that names no storage-node image either is a
// deployment nothing can build a DaemonSet for, and that has to fail loudly
// rather than build one with an empty image.
func TestImageErrorsWhenTheManagedControlPlaneNamesNoStorageNodeImage(t *testing.T) {
	scheme := testsupport.NewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(managedControlPlaneSingleton()).
		Build()
	r := &StorageNodeWorkloadReconciler{Client: c, Namespace: opsNamespace}

	if _, err := r.image(context.Background(), aStorageCluster()); err == nil {
		t.Error("image returned no error for a managed control plane naming no storage-node image")
	}
}
