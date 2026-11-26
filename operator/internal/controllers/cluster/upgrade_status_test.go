// The cluster facts a release upgrade reads off the StorageCluster: the
// write-protection generation and the images a node falls back to when nothing
// names one. Both are copied from the control plane's cluster record, from the
// stream once it has delivered its snapshot and from a direct read until then
// (design-release-upgrade.md §4.1, §5.1).

package cluster

import (
	"testing"

	atlascp "github.com/simplyblock/atlas/controlplane"
	"k8s.io/utils/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	testDefaultSpdkImage      = "quay.io/simplyblock-io/ultra:26.5.0"
	testDefaultSpdkProxyImage = "quay.io/simplyblock-io/simplyblock:26.5.0"
)

func TestTheStreamedWriteProtectionAndDefaultImagesReachTheStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		v2   *bool
		want simplyblockv1alpha2.WriteProtectionVersion
	}{
		{"switched", ptr.To(true), simplyblockv1alpha2.WriteProtectionV2},
		{"not switched", ptr.To(false), simplyblockv1alpha2.WriteProtectionV1},
		{"not reported", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeControlPlane{}
			refuseClusterReads(t, api)
			r := newClusterReconciler(t, api, &recorder{}, newTestCluster())
			r.Clusters = streamedCluster(func(dto *subscriptions.ClusterDTO) {
				dto.WriteProtectionV2 = tc.v2
				dto.DefaultSpdkImage = testDefaultSpdkImage
				dto.DefaultSpdkProxyImage = testDefaultSpdkProxyImage
			})

			cluster := reconcileCluster(t, r, 1)
			if cluster.Status.WriteProtection != tc.want {
				t.Errorf("writeProtection = %q, want %q", cluster.Status.WriteProtection, tc.want)
			}
			if cluster.Status.DefaultSpdkImage != testDefaultSpdkImage ||
				cluster.Status.DefaultSpdkProxyImage != testDefaultSpdkProxyImage {
				t.Errorf("default images = %q, %q, want the control plane's",
					cluster.Status.DefaultSpdkImage, cluster.Status.DefaultSpdkProxyImage)
			}
		})
	}
}

// Before the stream has its snapshot the record is read directly, and the
// fields must not be lost on that path either.
func TestTheDirectlyReadWriteProtectionAndDefaultImagesReachTheStatus(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) {
			c := activeCluster()
			c.WriteProtectionV2 = ptr.To(true)
			c.DefaultSpdkImage = testDefaultSpdkImage
			c.DefaultSpdkProxyImage = testDefaultSpdkProxyImage
			return c, nil
		},
	}
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster())
	r.Clusters = &fakeClusterCache{synced: false}

	cluster := reconcileCluster(t, r, 1)
	if cluster.Status.WriteProtection != simplyblockv1alpha2.WriteProtectionV2 {
		t.Errorf("writeProtection = %q, want V2", cluster.Status.WriteProtection)
	}
	if cluster.Status.DefaultSpdkImage != testDefaultSpdkImage ||
		cluster.Status.DefaultSpdkProxyImage != testDefaultSpdkProxyImage {
		t.Errorf("default images = %q, %q, want the control plane's",
			cluster.Status.DefaultSpdkImage, cluster.Status.DefaultSpdkProxyImage)
	}
}
