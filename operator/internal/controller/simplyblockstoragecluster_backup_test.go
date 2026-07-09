package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func TestBuildBackupConfig(t *testing.T) {
	snapshotBackups := true
	withCompression := false
	secondaryTarget := int32(0)
	localTesting := true

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add simplyblock scheme: %v", err)
	}

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sample-cluster",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.StorageClusterSpec{
			Backup: &simplyblockv1alpha1.BackupSpec{
				LocalEndpoint:   "http://10.10.11.10:9000",
				SnapshotBackups: &snapshotBackups,
				WithCompression: &withCompression,
				SecondaryTarget: &secondaryTarget,
				LocalTesting:    &localTesting,
				CredentialsSecretRef: simplyblockv1alpha1.BackupCredentialsSecretRef{
					Name: "sample-cluster-backup",
				},
			},
		},
	}

	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sample-cluster-backup",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"access_key_id":     []byte("username"),
			"secret_access_key": []byte("password"),
		},
	}

	reconciler := &StorageClusterReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, credentials).Build(),
		Scheme: scheme,
	}

	backupConfig, err := reconciler.buildBackupConfig(context.Background(), cluster)
	if err != nil {
		t.Fatalf("build backup config: %v", err)
	}

	if backupConfig == nil {
		t.Fatal("expected backup config, got nil")
	}
	if backupConfig.AccessKeyID != "username" {
		t.Fatalf("unexpected access_key_id: %#v", backupConfig.AccessKeyID)
	}
	if backupConfig.SecretAccessKey != "password" {
		t.Fatalf("unexpected secret_access_key: %#v", backupConfig.SecretAccessKey)
	}
	if backupConfig.LocalEndpoint != "http://10.10.11.10:9000" {
		t.Fatalf("unexpected local_endpoint: %#v", backupConfig.LocalEndpoint)
	}
	if backupConfig.SnapshotBackups == nil || *backupConfig.SnapshotBackups != true {
		t.Fatalf("unexpected snapshot_backups: %#v", backupConfig.SnapshotBackups)
	}
	if backupConfig.WithCompression == nil || *backupConfig.WithCompression != false {
		t.Fatalf("unexpected with_compression: %#v", backupConfig.WithCompression)
	}
	if backupConfig.SecondaryTarget == nil || *backupConfig.SecondaryTarget != 0 {
		t.Fatalf("unexpected secondary_target: %#v", backupConfig.SecondaryTarget)
	}
	if backupConfig.LocalTesting == nil || *backupConfig.LocalTesting != true {
		t.Fatalf("unexpected local_testing: %#v", backupConfig.LocalTesting)
	}
}

func TestBuildBackupConfigMissingSecretKey(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add simplyblock scheme: %v", err)
	}

	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sample-cluster",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.StorageClusterSpec{
			Backup: &simplyblockv1alpha1.BackupSpec{
				CredentialsSecretRef: simplyblockv1alpha1.BackupCredentialsSecretRef{
					Name: "sample-cluster-backup",
				},
			},
		},
	}

	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sample-cluster-backup",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"access_key_id": []byte("username"),
		},
	}

	reconciler := &StorageClusterReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, credentials).Build(),
		Scheme: scheme,
	}

	if _, err := reconciler.buildBackupConfig(context.Background(), cluster); err == nil {
		t.Fatal("expected error when secret_access_key is missing")
	}
}
