package controller

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const lvolUUID = "lvol-uuid"

func TestBackupRestoreEnsurePVIncludesCSIAttributes(t *testing.T) {
	scheme := newTestScheme(t, corev1.AddToScheme, simplyblockv1alpha1.AddToScheme)
	k8sClient := newTestClient(t, scheme, nil)

	apiClient := &webapi.Client{
		BaseURL: "http://simplyblock.test",
		HttpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet {
					t.Fatalf("method = %s, want %s", req.Method, http.MethodGet)
				}
				if req.URL.Path != "/api/v2/clusters/cluster-uuid/storage-pools/pool-uuid/volumes/lvol-uuid/connect" {
					t.Fatalf("path = %s", req.URL.Path)
				}
				if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
					t.Fatalf("authorization = %q", got)
				}

				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(`[
				{
					"nqn":"nqn.2026-04.io.simplyblock:cluster-uuid:lvol:lvol-uuid",
					"reconnect-delay":7,
					"nr-io-queues":3,
					"ctrl-loss-tmo":11,
					"port":4420,
					"transport":"tcp",
					"ip":"10.0.0.10",
					"ns_id":9,
					"host-iface":"ens1f0"
				},
				{
					"nqn":"nqn.2026-04.io.simplyblock:cluster-uuid:lvol:lvol-uuid",
					"reconnect-delay":7,
					"nr-io-queues":3,
					"ctrl-loss-tmo":11,
					"port":4420,
					"transport":"tcp",
					"ip":"10.0.0.11",
					"ns_id":9,
					"host-iface":"ens1f0"
				}
			]`)),
				}, nil
			}),
		},
	}

	r := &BackupRestoreReconciler{
		Client:    k8sClient,
		Scheme:    scheme,
		APIClient: apiClient,
	}

	restore := &simplyblockv1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-sample",
			Namespace: "default",
			UID:       "restore-uid",
		},
		Spec: simplyblockv1alpha1.BackupRestoreSpec{
			ClusterName: "mycluster",
			PVCTemplate: simplyblockv1alpha1.PVCTemplate{
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resourceMustParse(t, "10Gi"),
						},
					},
				},
			},
		},
		Status: simplyblockv1alpha1.BackupRestoreStatus{
			PoolName:       "pool-a",
			PoolUUID:       "pool-uuid",
			RestoredLvolID: "lvol-uuid",
		},
	}

	if err := r.ensurePV(
		context.Background(),
		restore,
		"restore-restore-uid",
		"restore-pvc",
		"default",
		"cluster-uuid",
	); err != nil {
		t.Fatalf("ensurePV returned error: %v", err)
	}

	pv := &corev1.PersistentVolume{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "restore-restore-uid"}, pv); err != nil {
		t.Fatalf("failed to get created PV: %v", err)
	}

	wantStorageClass := "simplyblock-default-mycluster-pool-a"
	if pv.Spec.StorageClassName != wantStorageClass {
		t.Fatalf("storageClassName = %q, want %q", pv.Spec.StorageClassName, wantStorageClass)
	}

	wantVolumeHandle := "cluster-uuid:pool-a:lvol-uuid"
	if pv.Spec.CSI.VolumeHandle != wantVolumeHandle {
		t.Fatalf("volumeHandle = %q, want %q", pv.Spec.CSI.VolumeHandle, wantVolumeHandle)
	}

	got := pv.Spec.CSI.VolumeAttributes
	if got["cluster_id"] != "cluster-uuid" {
		t.Fatalf("cluster_id = %q, want %q", got["cluster_id"], "cluster-uuid")
	}
	if got["targetType"] != "tcp" {
		t.Fatalf("targetType = %q, want %q", got["targetType"], "tcp")
	}
	if got["nqn"] != "nqn.2026-04.io.simplyblock:cluster-uuid:lvol:lvol-uuid" {
		t.Fatalf("nqn = %q", got["nqn"])
	}
	if got["connections"] != `[{"ip":"10.0.0.10","port":4420},{"ip":"10.0.0.11","port":4420}]` {
		t.Fatalf("connections = %q", got["connections"])
	}
	if got["reconnectDelay"] != "7" || got["nrIoQueues"] != "3" || got["ctrlLossTmo"] != "11" || got["nsId"] != "9" {
		t.Fatalf("unexpected numeric CSI attrs: %#v", got)
	}
	if got["hostIface"] != "ens1f0" {
		t.Fatalf("hostIface = %q, want %q", got["hostIface"], "ens1f0")
	}
	if got["uuid"] != lvolUUID || got["name"] != lvolUUID || got["model"] != lvolUUID {
		t.Fatalf("unexpected identity CSI attrs: %#v", got)
	}
}

// TestBackupRestoreFailsWhenBackupIsFailed verifies that a BackupRestore referencing a
// StorageBackup stuck in a terminal Failed phase is itself marked Failed (with no further
// requeue), rather than looping in Pending forever.
func TestBackupRestoreFailsWhenBackupIsFailed(t *testing.T) {
	scheme := newTestScheme(t, corev1.AddToScheme, simplyblockv1alpha1.AddToScheme)

	cluster := testCluster("default", "mycluster", "cluster-uuid")
	backup := &simplyblockv1alpha1.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-sample",
			Namespace: "default",
		},
		Status: simplyblockv1alpha1.StorageBackupStatus{
			Phase:   simplyblockv1alpha1.BackupPhaseFailed,
			Message: "Snapshot snap-1 not found",
		},
	}
	restore := &simplyblockv1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-sample",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.BackupRestoreSpec{
			ClusterName: "mycluster",
			BackupRef:   simplyblockv1alpha1.BackupRef{Name: "backup-sample"},
			PVCTemplate: simplyblockv1alpha1.PVCTemplate{
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resourceMustParse(t, "10Gi"),
						},
					},
				},
			},
		},
	}

	k8sClient := newTestClient(t, scheme,
		[]client.Object{&simplyblockv1alpha1.BackupRestore{}},
		cluster, backup, restore,
	)

	r := &BackupRestoreReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(10),
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "restore-sample", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want 0 (restore must terminate, not poll forever)", res.RequeueAfter)
	}

	got := &simplyblockv1alpha1.BackupRestore{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "restore-sample", Namespace: "default"}, got); err != nil {
		t.Fatalf("failed to get restore: %v", err)
	}
	if got.Status.Phase != simplyblockv1alpha1.RestorePhaseFailed {
		t.Fatalf("Phase = %q, want %q", got.Status.Phase, simplyblockv1alpha1.RestorePhaseFailed)
	}
	if !strings.Contains(got.Status.Message, "Snapshot snap-1 not found") {
		t.Fatalf("Message = %q, want it to include the backup's failure reason", got.Status.Message)
	}
}

func TestResolveCrossClusterCredentialsLocalRestoreReturnsNil(t *testing.T) {
	scheme := newTestScheme(t, corev1.AddToScheme, simplyblockv1alpha1.AddToScheme)
	k8sClient := newTestClient(t, scheme, nil)
	r := &BackupRestoreReconciler{Client: k8sClient, Scheme: scheme}

	restore := &simplyblockv1alpha1.BackupRestore{
		Status: simplyblockv1alpha1.BackupRestoreStatus{
			ClusterUUID:       "cluster-uuid",
			SourceClusterUUID: "cluster-uuid",
		},
	}

	creds, err := r.resolveCrossClusterCredentials(context.Background(), restore)
	if err != nil {
		t.Fatalf("resolveCrossClusterCredentials returned error: %v", err)
	}
	if creds != nil {
		t.Fatalf("creds = %+v, want nil for a local restore", creds)
	}
}

func TestResolveCrossClusterCredentialsResolvesSourceClusterSecret(t *testing.T) {
	scheme := newTestScheme(t, corev1.AddToScheme, simplyblockv1alpha1.AddToScheme)

	sourceCluster := testCluster("default", "source-cluster", "source-uuid")
	sourceCluster.Spec.Backup = &simplyblockv1alpha1.BackupSpec{
		CredentialsSecretRef: simplyblockv1alpha1.BackupCredentialsSecretRef{Name: "source-backup-creds"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "source-backup-creds", Namespace: "default"},
		Data: map[string][]byte{
			"access_key_id":     []byte("AKIDEXAMPLE"),
			"secret_access_key": []byte("supersecret"),
		},
	}

	k8sClient := newTestClient(t, scheme, nil, sourceCluster, secret)
	r := &BackupRestoreReconciler{Client: k8sClient, Scheme: scheme}

	restore := &simplyblockv1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Status: simplyblockv1alpha1.BackupRestoreStatus{
			ClusterUUID:       "target-uuid",
			SourceClusterUUID: "source-uuid",
		},
	}

	creds, err := r.resolveCrossClusterCredentials(context.Background(), restore)
	if err != nil {
		t.Fatalf("resolveCrossClusterCredentials returned error: %v", err)
	}
	if creds == nil {
		t.Fatal("creds = nil, want resolved credentials")
	}
	if creds.AccessKeyID != "AKIDEXAMPLE" || creds.SecretAccessKey != "supersecret" {
		t.Fatalf("creds = %+v, want AKIDEXAMPLE/supersecret", creds)
	}
}

func TestResolveCrossClusterCredentialsSourceClusterGone(t *testing.T) {
	scheme := newTestScheme(t, corev1.AddToScheme, simplyblockv1alpha1.AddToScheme)
	k8sClient := newTestClient(t, scheme, nil)
	r := &BackupRestoreReconciler{Client: k8sClient, Scheme: scheme}

	restore := &simplyblockv1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Status: simplyblockv1alpha1.BackupRestoreStatus{
			ClusterUUID:       "target-uuid",
			SourceClusterUUID: "source-uuid",
		},
	}

	creds, err := r.resolveCrossClusterCredentials(context.Background(), restore)
	if err == nil {
		t.Fatal("expected an error when the source cluster CR no longer exists")
	}
	if creds != nil {
		t.Fatalf("creds = %+v, want nil alongside the error", creds)
	}
}

func TestResolveCrossClusterCredentialsSourceClusterHasNoBackupConfig(t *testing.T) {
	scheme := newTestScheme(t, corev1.AddToScheme, simplyblockv1alpha1.AddToScheme)
	sourceCluster := testCluster("default", "source-cluster", "source-uuid")
	k8sClient := newTestClient(t, scheme, nil, sourceCluster)
	r := &BackupRestoreReconciler{Client: k8sClient, Scheme: scheme}

	restore := &simplyblockv1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Status: simplyblockv1alpha1.BackupRestoreStatus{
			ClusterUUID:       "target-uuid",
			SourceClusterUUID: "source-uuid",
		},
	}

	if _, err := r.resolveCrossClusterCredentials(context.Background(), restore); err == nil {
		t.Fatal("expected an error when the source cluster has no backup configuration")
	}
}

func resourceMustParse(t *testing.T, value string) resource.Quantity {
	t.Helper()

	q, err := resource.ParseQuantity(value)
	if err != nil {
		t.Fatalf("ParseQuantity(%q) failed: %v", value, err)
	}
	return q
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// Regression: 2026-09-02-backuprestore-never-terminates. Every reason a restore could
// stall before the control plane accepted it requeued at 10s forever, so the
// BackupRestore never left Pending and callers waiting on a terminal phase hung. One
// full cluster held 124 of them, costing 2,301 retries in 29 minutes. This drives the
// missing-StorageBackup path, which accounted for 15 of those.
func TestBackupRestoreGivesUpAfterRepeatedAttempts(t *testing.T) {
	scheme := newTestScheme(t, corev1.AddToScheme, simplyblockv1alpha1.AddToScheme)

	cluster := testCluster("default", "mycluster", "cluster-uuid")
	restore := &simplyblockv1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-sample", Namespace: "default"},
		Spec: simplyblockv1alpha1.BackupRestoreSpec{
			ClusterName: "mycluster",
			// No StorageBackup of this name exists.
			BackupRef: simplyblockv1alpha1.BackupRef{Name: "backup-that-never-existed"},
		},
	}

	k8sClient := newTestClient(t, scheme,
		[]client.Object{&simplyblockv1alpha1.BackupRestore{}}, cluster, restore)
	r := &BackupRestoreReconciler{Client: k8sClient, Scheme: scheme, Recorder: events.NewFakeRecorder(64)}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "restore-sample", Namespace: "default"}}
	key := client.ObjectKey{Name: "restore-sample", Namespace: "default"}

	for i := 1; i <= maxRestoreAttempts; i++ {
		res, err := r.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("attempt %d returned error: %v", i, err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("attempt %d gave up early, want a requeue while attempts remain", i)
		}
	}

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v after giving up, want 0", res.RequeueAfter)
	}

	got := &simplyblockv1alpha1.BackupRestore{}
	if err := k8sClient.Get(context.Background(), key, got); err != nil {
		t.Fatalf("failed to get restore: %v", err)
	}
	if got.Status.Phase != simplyblockv1alpha1.RestorePhaseFailed {
		t.Fatalf("Phase = %q after %d attempts, want %q",
			got.Status.Phase, maxRestoreAttempts, simplyblockv1alpha1.RestorePhaseFailed)
	}
	if !strings.Contains(got.Status.Message, "backup-that-never-existed") {
		t.Fatalf("Message = %q, want it to keep the reason the restore stalled", got.Status.Message)
	}

	// A terminal restore re-reconciles to nothing.
	if res, err = r.Reconcile(context.Background(), req); err != nil || res.RequeueAfter != 0 {
		t.Fatalf("Reconcile of a Failed restore = (%v, %v), want no-op", res, err)
	}
}

// Regression: 2026-09-02-backuprestore-counts-reconciles-not-failures. The budget was
// spent at the top of Reconcile, before the pass did any work, so a restore whose backup
// became ready on the last attempt was failed without the restore ever being tried.
func TestBackupRestoreAcceptsARestoreOnItsLastAttempt(t *testing.T) {
	scheme := newTestScheme(t, corev1.AddToScheme, simplyblockv1alpha1.AddToScheme)

	cluster := testCluster("default", "mycluster", "cluster-uuid")
	restore := &simplyblockv1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-sample", Namespace: "default", UID: "restore-uid"},
		Spec: simplyblockv1alpha1.BackupRestoreSpec{
			ClusterName: "mycluster",
			// Not there yet; it arrives below, on the last attempt.
			BackupRef: simplyblockv1alpha1.BackupRef{Name: "late-backup"},
			PVCTemplate: simplyblockv1alpha1.PVCTemplate{
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resourceMustParse(t, "10Gi"),
						},
					},
				},
			},
		},
	}

	k8sClient := newTestClient(t, scheme,
		[]client.Object{&simplyblockv1alpha1.BackupRestore{}}, cluster, restore)

	restoreCalls := 0
	apiClient := &webapi.Client{
		BaseURL: "http://simplyblock.test",
		HttpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body := `{"status":"restoring"}`
				if strings.HasSuffix(req.URL.Path, "/backups/restore") {
					restoreCalls++
					body = `{"lvol_id":"restored-lvol"}`
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}),
		},
	}

	r := &BackupRestoreReconciler{
		Client: k8sClient, Scheme: scheme,
		Recorder: events.NewFakeRecorder(64), APIClient: apiClient,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "restore-sample", Namespace: "default"}}
	key := client.ObjectKey{Name: "restore-sample", Namespace: "default"}

	// Spend every attempt but the last with the backup still missing.
	for i := 1; i <= maxRestoreAttempts; i++ {
		res, err := r.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("attempt %d returned error: %v", i, err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("attempt %d gave up early, want a requeue while attempts remain", i)
		}
	}

	// The backup finishes, so the next pass can place the restore.
	backup := &simplyblockv1alpha1.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "late-backup", Namespace: "default"},
		Status: simplyblockv1alpha1.StorageBackupStatus{
			Phase:    simplyblockv1alpha1.BackupPhaseDone,
			BackupID: "backup-id",
			PoolName: "pool-a",
			PoolUUID: "pool-uuid",
			LvolID:   "source-lvol",
			Size:     1 << 30,
		},
	}
	if err := k8sClient.Create(context.Background(), backup); err != nil {
		t.Fatalf("failed to create the backup: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	if restoreCalls != 1 {
		t.Fatalf("restore API calls = %d, want 1: the attempt that could succeed must be tried", restoreCalls)
	}

	got := &simplyblockv1alpha1.BackupRestore{}
	if err := k8sClient.Get(context.Background(), key, got); err != nil {
		t.Fatalf("failed to get restore: %v", err)
	}
	if got.Status.Phase != simplyblockv1alpha1.RestorePhaseInProgress {
		t.Fatalf("Phase = %q, want %q: the restore was accepted, not exhausted",
			got.Status.Phase, simplyblockv1alpha1.RestorePhaseInProgress)
	}
	if got.Status.RestoredLvolID != "restored-lvol" {
		t.Fatalf("RestoredLvolID = %q, want the accepted lvol", got.Status.RestoredLvolID)
	}
}
