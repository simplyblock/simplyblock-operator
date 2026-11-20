// The network interfaces a cluster is built on, and why the apiserver refuses to
// change them.
//
// Both are spent once, on a call whose result the operator never revisits: the
// node-add carries the management and data NICs of the node it is adding, and the
// control plane resolves them into that node's own record there. A later edit
// reconfigures nothing, so the object would say one thing while every node built
// from it did another.
//
// spec.clientDataIfname is deliberately not here. It reads like a third member of
// the set and is not one: the control plane renders it as --host-iface= into every
// `nvme connect` for the life of the cluster, so editing it is how a fleet moves onto
// a different client data network.

package cluster

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// storageNodes.dataInterfaces reaches the control plane as the node-add's
// data_nics, beside the mgmtInterface that is already immutable for the same
// reason. Changing it after a node is added leaves that node on the NICs it
// joined with, so the two belong under one rule rather than one each.
func TestTheDataInterfacesAreImmutableOnceSet(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		initial []string
		mutate  func(*simplyblockv1alpha2.StorageNodesSpec)
		wantErr string
	}{
		{
			name:    "changing the list",
			initial: []string{"eth2"},
			mutate: func(w *simplyblockv1alpha2.StorageNodesSpec) {
				w.DataInterfaces = []string{"eth3"}
			},
			wantErr: "field is immutable",
		},
		{
			name:    "appending to the list",
			initial: []string{"eth2"},
			mutate: func(w *simplyblockv1alpha2.StorageNodesSpec) {
				w.DataInterfaces = []string{"eth2", "eth3"}
			},
			wantErr: "field is immutable",
		},
		{
			name:    "clearing the list",
			initial: []string{"eth2"},
			mutate: func(w *simplyblockv1alpha2.StorageNodesSpec) {
				w.DataInterfaces = nil
			},
			wantErr: "immutable once set",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &simplyblockv1alpha2.StorageCluster{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "datanics-", Namespace: "default"},
				Spec: simplyblockv1alpha2.StorageClusterSpec{
					MaxSubsystemCount: ptr.To(int32(10)),
					VCPUCount:         ptr.To(int32(6)),
					StorageNodes: &simplyblockv1alpha2.StorageNodesSpec{
						DataInterfaces: tc.initial,
					},
				},
			}
			if err := apiClient.Create(ctx, cluster); err != nil {
				t.Fatalf("creating the cluster: %v", err)
			}
			t.Cleanup(func() { _ = apiClient.Delete(ctx, cluster) })

			tc.mutate(cluster.Spec.StorageNodes)
			err := apiClient.Update(ctx, cluster)
			if err == nil {
				t.Fatal("the apiserver accepted the change")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}
}

// A cluster created without data interfaces can still be given them, which is
// what once-set means and what keeps the marker from freezing a field nobody has
// filled in yet.
func TestTheDataInterfacesCanBeSetOnceOnAClusterThatOmittedThem(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "datanics-late-", Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(10)),
			VCPUCount:         ptr.To(int32(6)),
			StorageNodes:      &simplyblockv1alpha2.StorageNodesSpec{},
		},
	}
	if err := apiClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating the cluster: %v", err)
	}
	t.Cleanup(func() { _ = apiClient.Delete(ctx, cluster) })

	cluster.Spec.StorageNodes.DataInterfaces = []string{"eth2"}
	if err := apiClient.Update(ctx, cluster); err != nil {
		t.Fatalf("the first assignment was refused: %v", err)
	}
}
