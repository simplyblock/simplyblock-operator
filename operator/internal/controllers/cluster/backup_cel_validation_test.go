// Validation of the CEL rules compiled into the StorageCluster CRD for
// spec.backup's two credential spellings, run against a real apiserver like
// the rest of this package's CEL suite. The rules decide how a store's
// credentials may be stated: a Secret, or the AWS provider chain, never both
// and never neither, and never alongside the deprecated
// credentialsSecretRef.

package cluster

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func clusterWithBackup(store *simplyblockv1alpha2.BackupStoreSpec) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "backup-", Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(10)),
			VCPUCount:         ptr.To(int32(6)),
			Backup:            store,
		},
	}
}

func TestBackupStoreCredentialsSourceRules(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name   string
		modify func(*simplyblockv1alpha2.BackupStoreSpec)
		reason string
	}{
		{
			name: "the deprecated credentialsSecretRef alone",
			modify: func(s *simplyblockv1alpha2.BackupStoreSpec) {
				//nolint:staticcheck // SA1019: exercising the deprecated field's own admission rule
				s.CredentialsSecretRef = &corev1.LocalObjectReference{Name: "creds"}
			},
		},
		{
			name: "credentials naming Secret with a secretRef",
			modify: func(s *simplyblockv1alpha2.BackupStoreSpec) {
				s.Credentials = &simplyblockv1alpha2.BackupCredentials{
					Source:    simplyblockv1alpha2.BackupCredentialsSourceSecret,
					SecretRef: &corev1.LocalObjectReference{Name: "creds"},
				}
			},
		},
		{
			name: "credentials naming ProviderChain with no secretRef",
			modify: func(s *simplyblockv1alpha2.BackupStoreSpec) {
				s.Credentials = &simplyblockv1alpha2.BackupCredentials{
					Source: simplyblockv1alpha2.BackupCredentialsSourceProviderChain,
				}
			},
		},
		{
			name:   "neither credentials nor credentialsSecretRef",
			modify: func(*simplyblockv1alpha2.BackupStoreSpec) {},
			reason: "one of credentials and the deprecated credentialsSecretRef must be set",
		},
		{
			name: "both credentials and credentialsSecretRef",
			modify: func(s *simplyblockv1alpha2.BackupStoreSpec) {
				//nolint:staticcheck // SA1019: exercising the deprecated field's own admission rule
				s.CredentialsSecretRef = &corev1.LocalObjectReference{Name: "creds"}
				s.Credentials = &simplyblockv1alpha2.BackupCredentials{
					Source: simplyblockv1alpha2.BackupCredentialsSourceProviderChain,
				}
			},
			reason: "credentials and the deprecated credentialsSecretRef are exclusive",
		},
		{
			name: "Secret with no secretRef",
			modify: func(s *simplyblockv1alpha2.BackupStoreSpec) {
				s.Credentials = &simplyblockv1alpha2.BackupCredentials{
					Source: simplyblockv1alpha2.BackupCredentialsSourceSecret,
				}
			},
			reason: "source: Secret requires secretRef",
		},
		{
			name: "ProviderChain with a secretRef",
			modify: func(s *simplyblockv1alpha2.BackupStoreSpec) {
				s.Credentials = &simplyblockv1alpha2.BackupCredentials{
					Source:    simplyblockv1alpha2.BackupCredentialsSourceProviderChain,
					SecretRef: &corev1.LocalObjectReference{Name: "creds"},
				}
			},
			reason: "source: ProviderChain forbids secretRef",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &simplyblockv1alpha2.BackupStoreSpec{
				Endpoint: "https://s3.example.com",
				Bucket:   "backups",
			}
			tc.modify(store)
			err := create(t, apiClient, clusterWithBackup(store))
			if tc.reason == "" {
				wantAccepted(t, err)
				return
			}
			wantRejected(t, err, tc.reason)
		})
	}
}
