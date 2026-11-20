// Admission tests for spec.multiAttach: what a multi-attach cluster requires
// of the rest of its spec, which of its fields are fixed at creation, and how
// the array's credential source and its fields must agree.

package cluster

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func multiAttachCluster(mutate func(*simplyblockv1alpha2.StorageClusterSpec)) *simplyblockv1alpha2.StorageCluster {
	spec := simplyblockv1alpha2.StorageClusterSpec{
		MaxSubsystemCount: ptr.To(int32(10)),
		VCPUCount:         ptr.To(int32(6)),
		DeviceClass:       simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock,
		RaidDevices:       ptr.To(true),
		MultiAttach: &simplyblockv1alpha2.MultiAttachSpec{
			FaultTolerance: 1,
			Array: &simplyblockv1alpha2.StorageArraySpec{
				Plugin:   "pure-flasharray",
				Endpoint: "https://pure01.example",
				Protocol: simplyblockv1alpha2.StorageArrayProtocolISCSI,
				LunSize:  resource.MustParse("4Ti"),
				Credentials: simplyblockv1alpha2.StorageArrayCredentials{
					Source:    simplyblockv1alpha2.StorageArrayCredentialsSourceSecret,
					SecretRef: &corev1.LocalObjectReference{Name: "pure01"},
				},
			},
		},
	}
	if mutate != nil {
		mutate(&spec)
	}
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-ma-", Namespace: "default"},
		Spec:       spec,
	}
}

func TestStorageClusterCELValidatesMultiAttach(t *testing.T) {
	apiClient := apiServer(t)
	const requires = "multiAttach requires raidDevices"

	for _, tc := range []struct {
		name       string
		mutate     func(*simplyblockv1alpha2.StorageClusterSpec)
		wantDenied string
	}{
		{name: "a complete multi-attach cluster"},
		{name: "without an array", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach.Array = nil
		}},
		{name: "login in Vault", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach.Array.Credentials = simplyblockv1alpha2.StorageArrayCredentials{
				Source:  simplyblockv1alpha2.StorageArrayCredentialsSourceVaultKV,
				VaultKV: &simplyblockv1alpha2.StorageArrayVaultKVSecret{Mount: "secret", Path: "arrays/pure01"},
			}
		}},
		{name: "without raidDevices", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.RaidDevices = nil
		}, wantDenied: requires},
		{name: "on NVMe devices", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.DeviceClass = simplyblockv1alpha2.StorageClusterDeviceClassNVMe
		}, wantDenied: requires},
		{name: "single node", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.ClusterMode = simplyblockv1alpha2.ClusterModeSingle
		}, wantDenied: requires},
		{name: "fault tolerance 3", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach.FaultTolerance = 3
		}, wantDenied: "faultTolerance"},
		{name: "Secret source without a secretRef", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach.Array.Credentials.SecretRef = nil
		}, wantDenied: "secretRef is set exactly when source is Secret"},
		{name: "a secretRef beside a Vault source", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach.Array.Credentials.Source = simplyblockv1alpha2.StorageArrayCredentialsSourceVaultKV
			s.MultiAttach.Array.Credentials.VaultKV = &simplyblockv1alpha2.StorageArrayVaultKVSecret{Mount: "m", Path: "p"}
		}, wantDenied: "secretRef is set exactly when source is Secret"},
		{name: "unknown protocol", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach.Array.Protocol = "smoke-signals"
		}, wantDenied: "protocol"},
		{name: "journal LUN in a fast pool", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			size := resource.MustParse("64Gi")
			s.MultiAttach.Array.Journal = &simplyblockv1alpha2.StorageArrayJournal{
				Placement: simplyblockv1alpha2.StorageArrayJournalLUN, Size: &size,
				Options: map[string]string{"pool": "fast"}}
		}},
		{name: "local journal", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach.Array.Journal = &simplyblockv1alpha2.StorageArrayJournal{
				Placement: simplyblockv1alpha2.StorageArrayJournalLocal}
		}},
		{name: "local journal with a pool", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach.Array.Journal = &simplyblockv1alpha2.StorageArrayJournal{
				Placement: simplyblockv1alpha2.StorageArrayJournalLocal, Options: map[string]string{"pool": "fast"}}
		}, wantDenied: "size and options apply to the placement LUN only"},
		{name: "local key store for a Secret", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach.Array.Credentials.AllowLocalKeyStore = ptr.To(true)
		}},
		{name: "local key store for a Vault login", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach.Array.Credentials = simplyblockv1alpha2.StorageArrayCredentials{
				Source:             simplyblockv1alpha2.StorageArrayCredentialsSourceVaultKV,
				VaultKV:            &simplyblockv1alpha2.StorageArrayVaultKVSecret{Mount: "secret", Path: "p"},
				AllowLocalKeyStore: ptr.To(true),
			}
		}, wantDenied: "allowLocalKeyStore applies to the source Secret only"},
		{name: "unknown journal placement", mutate: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach.Array.Journal = &simplyblockv1alpha2.StorageArrayJournal{Placement: "NVRAM"}
		}, wantDenied: "placement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := multiAttachCluster(tc.mutate)
			err := apiClient.Create(context.Background(), cluster)
			if tc.wantDenied != "" {
				if err == nil {
					t.Fatal("expected the apiserver to reject the cluster, but it was accepted")
				}
				if !strings.Contains(err.Error(), tc.wantDenied) {
					t.Fatalf("rejected for the wrong reason: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected the apiserver to accept the cluster, got: %v", err)
			}
			_ = apiClient.Delete(context.Background(), cluster)
		})
	}
}

// The mode and its fault tolerance are fixed at creation; the array block is
// not, because an endpoint moves and a login is rotated.
func TestStorageClusterCELFixesMultiAttachAtCreation(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		start      *simplyblockv1alpha2.StorageCluster
		edit       func(*simplyblockv1alpha2.StorageClusterSpec)
		wantDenied string
	}{
		{name: "array endpoint moves", start: multiAttachCluster(nil),
			edit: func(s *simplyblockv1alpha2.StorageClusterSpec) {
				s.MultiAttach.Array.Endpoint = "https://pure02.example"
			}},
		{name: "fault tolerance changes", start: multiAttachCluster(nil),
			edit: func(s *simplyblockv1alpha2.StorageClusterSpec) {
				s.MultiAttach.FaultTolerance = 2
			}, wantDenied: "fixed when the cluster is created"},
		{name: "multiAttach removed", start: multiAttachCluster(nil),
			edit: func(s *simplyblockv1alpha2.StorageClusterSpec) {
				s.MultiAttach = nil
			}, wantDenied: "fixed when the cluster is created"},
		{name: "multiAttach added", start: multiAttachCluster(func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach = nil
		}), edit: func(s *simplyblockv1alpha2.StorageClusterSpec) {
			s.MultiAttach = &simplyblockv1alpha2.MultiAttachSpec{FaultTolerance: 1}
		}, wantDenied: "fixed when the cluster is created"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := tc.start
			if err := apiClient.Create(ctx, cluster); err != nil {
				t.Fatalf("create: %v", err)
			}
			defer func() { _ = apiClient.Delete(ctx, cluster) }()
			tc.edit(&cluster.Spec)
			err := apiClient.Update(ctx, cluster)
			if tc.wantDenied != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantDenied) {
					t.Fatalf("update err = %v, want a refusal containing %q", err, tc.wantDenied)
				}
				return
			}
			if err != nil {
				t.Fatalf("update refused: %v", err)
			}
		})
	}
}
