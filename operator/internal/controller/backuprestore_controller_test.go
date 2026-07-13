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
	"k8s.io/client-go/tools/record"
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
		Recorder: record.NewFakeRecorder(10),
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
