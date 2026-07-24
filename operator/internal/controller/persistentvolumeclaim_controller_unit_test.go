package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	pinNamespace   = "sb"
	pinClusterNS   = "sb-system" // StorageCluster CR namespace (deliberately != PVC namespace)
	pinClusterName = "sb-cluster"
	pinPVCName     = "data-pvc"
	pinPVName      = "pv-data"
	pinCluster     = "cluster-uuid"
	pinPool        = "pool-uuid"
	pinVolume      = "vol-uuid"
	pinNodeA       = "node-a"
	pinNodeB       = "node-b"
)

// pinClusterCR is the StorageCluster CR whose reported UUID matches the PV's
// cluster. It lives in pinClusterNS, distinct from the PVC namespace, to prove
// the migration is created alongside the cluster CR rather than the PVC.
func pinClusterCR() *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: pinClusterName, Namespace: pinClusterNS},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: pinCluster},
	}
}

// pinAPIServer serves the two control-plane endpoints the PVC controller uses:
// the storage-node list (for target validation) and a single volume (for current
// placement). currentNode is the storage_node_id reported for the volume.
func pinAPIServer(t *testing.T, nodes []string, currentNode string) string {
	t.Helper()
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/storage-nodes/"):
			var b strings.Builder
			b.WriteString("[")
			for i, n := range nodes {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"id":"` + n + `"}`)
			}
			b.WriteString("]")
			_, _ = w.Write([]byte(b.String()))
		case strings.Contains(r.URL.Path, "/volumes/"):
			_, _ = w.Write([]byte(`{"id":"` + pinVolume + `","storage_node_id":"` + currentNode + `"}`))
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	})
	return srv.URL
}

func newPVCReconciler(t *testing.T, apiURL string, objs ...client.Object) (*PersistentVolumeClaimReconciler, client.Client) {
	t.Helper()
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, nil, objs...)
	r := &PersistentVolumeClaimReconciler{
		Client:    cl,
		Scheme:    scheme,
		Recorder:  events.NewFakeRecorder(64),
		apiClient: webapi.NewClient(apiURL),
	}
	return r, cl
}

func pinPV() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pinPVName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "csi.simplyblock.io",
					VolumeHandle: pinCluster + ":" + pinPool + ":" + pinVolume,
				},
			},
		},
	}
}

// pinPVC builds a bound PVC with the given pinned/applied annotation values
// ("" omits the annotation).
func pinPVC(pinned, applied string) *corev1.PersistentVolumeClaim {
	ann := map[string]string{}
	if pinned != "" {
		ann[simplyblockv1alpha1.AnnotationPinnedVolume] = pinned
	}
	if applied != "" {
		ann[simplyblockv1alpha1.AnnotationPinnedVolumeApplied] = applied
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pinPVCName, Namespace: pinNamespace, Annotations: ann},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pinPVName},
	}
}

func pinRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pinNamespace, Name: pinPVCName}}
}

func getPinPVC(t *testing.T, cl client.Client) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: pinNamespace, Name: pinPVCName}, pvc); err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	return pvc
}

func listPinMigrations(t *testing.T, cl client.Client) []simplyblockv1alpha1.VolumeMigration {
	t.Helper()
	var list simplyblockv1alpha1.VolumeMigrationList
	if err := cl.List(context.Background(), &list); err != nil {
		t.Fatalf("list VolumeMigrations: %v", err)
	}
	return list.Items
}

func TestPVCReconcile_NoChangeGate(t *testing.T) {
	// desired == applied → nothing happens, no API call.
	r, cl := newPVCReconciler(t, unreachableAPI, pinPVC(pinNodeB, pinNodeB), pinPV())
	if _, err := r.Reconcile(context.Background(), pinRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(listPinMigrations(t, cl)); got != 0 {
		t.Fatalf("expected no migrations, got %d", got)
	}
}

func TestPVCReconcile_Unpin(t *testing.T) {
	// Annotation removed but applied still set → clear applied, no migration.
	r, cl := newPVCReconciler(t, unreachableAPI, pinPVC("", pinNodeB), pinPV())
	if _, err := r.Reconcile(context.Background(), pinRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pvc := getPinPVC(t, cl)
	if _, ok := pvc.Annotations[simplyblockv1alpha1.AnnotationPinnedVolumeApplied]; ok {
		t.Fatalf("expected applied annotation cleared, still present")
	}
	if got := len(listPinMigrations(t, cl)); got != 0 {
		t.Fatalf("expected no migrations, got %d", got)
	}
}

func TestPVCReconcile_UnboundRequeues(t *testing.T) {
	pvc := pinPVC(pinNodeB, "")
	pvc.Spec.VolumeName = ""
	r, cl := newPVCReconciler(t, unreachableAPI, pvc)
	res, err := r.Reconcile(context.Background(), pinRequest())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue for unbound PVC")
	}
	if got := len(listPinMigrations(t, cl)); got != 0 {
		t.Fatalf("expected no migrations, got %d", got)
	}
}

func TestPVCReconcile_InvalidTarget(t *testing.T) {
	// desired is not among the cluster's storage nodes → reject, no migration.
	api := pinAPIServer(t, []string{pinNodeA, pinNodeB}, pinNodeA)
	r, cl := newPVCReconciler(t, api, pinPVC("ghost-node", ""), pinPV())
	if _, err := r.Reconcile(context.Background(), pinRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(listPinMigrations(t, cl)); got != 0 {
		t.Fatalf("expected no migrations for invalid target, got %d", got)
	}
	pvc := getPinPVC(t, cl)
	if pvc.Annotations[simplyblockv1alpha1.AnnotationPinnedVolumeRejected] != "ghost-node" {
		t.Fatalf("expected rejected annotation = ghost-node, got %q",
			pvc.Annotations[simplyblockv1alpha1.AnnotationPinnedVolumeRejected])
	}
	if _, ok := pvc.Annotations[simplyblockv1alpha1.AnnotationPinnedVolumeApplied]; ok {
		t.Fatalf("applied must not be set for a rejected target")
	}
}

func TestPVCReconcile_AlreadyOnTarget(t *testing.T) {
	// Volume already lives on the requested node → mark applied, no migration.
	api := pinAPIServer(t, []string{pinNodeA, pinNodeB}, pinNodeB)
	r, cl := newPVCReconciler(t, api, pinPVC(pinNodeB, ""), pinPV())
	if _, err := r.Reconcile(context.Background(), pinRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(listPinMigrations(t, cl)); got != 0 {
		t.Fatalf("expected no migrations, got %d", got)
	}
	if getPinPVC(t, cl).Annotations[simplyblockv1alpha1.AnnotationPinnedVolumeApplied] != pinNodeB {
		t.Fatalf("expected applied = %s", pinNodeB)
	}
}

func TestPVCReconcile_ValidChangeCreatesMigration(t *testing.T) {
	// Volume on node-a, pin to node-b → create migration + record applied.
	api := pinAPIServer(t, []string{pinNodeA, pinNodeB}, pinNodeA)
	r, cl := newPVCReconciler(t, api, pinPVC(pinNodeB, ""), pinPV(), pinClusterCR())
	if _, err := r.Reconcile(context.Background(), pinRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	migs := listPinMigrations(t, cl)
	if len(migs) != 1 {
		t.Fatalf("expected 1 migration, got %d", len(migs))
	}
	m := migs[0]
	if m.Spec.PVName != pinPVName || m.Spec.TargetNodeUUID != pinNodeB {
		t.Fatalf("unexpected migration spec: %+v", m.Spec)
	}
	// The migration must be created in the StorageCluster's namespace, not the PVC's.
	if m.Namespace != pinClusterNS {
		t.Fatalf("expected migration in cluster namespace %q, got %q", pinClusterNS, m.Namespace)
	}
	if m.Labels[labelPinnedVolumePV] != pinPVLabelValue(pinPVName) {
		t.Fatalf("expected PV label %q, got %q", pinPVLabelValue(pinPVName), m.Labels[labelPinnedVolumePV])
	}
	if len(m.OwnerReferences) != 1 || m.OwnerReferences[0].Name != pinClusterName ||
		m.OwnerReferences[0].Kind != "StorageCluster" {
		t.Fatalf("expected owner reference to StorageCluster, got %+v", m.OwnerReferences)
	}
	if getPinPVC(t, cl).Annotations[simplyblockv1alpha1.AnnotationPinnedVolumeApplied] != pinNodeB {
		t.Fatalf("expected applied = %s after creating migration", pinNodeB)
	}
}

func TestPVCReconcile_NoStorageCluster(t *testing.T) {
	// Valid target and a volume that needs moving, but no StorageCluster CR
	// manages the cluster → no migration, requeue, applied left unset.
	api := pinAPIServer(t, []string{pinNodeA, pinNodeB}, pinNodeA)
	r, cl := newPVCReconciler(t, api, pinPVC(pinNodeB, ""), pinPV())
	res, err := r.Reconcile(context.Background(), pinRequest())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue when no StorageCluster manages the cluster")
	}
	if got := len(listPinMigrations(t, cl)); got != 0 {
		t.Fatalf("expected no migrations, got %d", got)
	}
	if _, ok := getPinPVC(t, cl).Annotations[simplyblockv1alpha1.AnnotationPinnedVolumeApplied]; ok {
		t.Fatalf("applied must not be set when the migration could not be created")
	}
}

func TestPVCReconcile_ActiveMigrationWaits(t *testing.T) {
	// A non-terminal migration for this PV already exists → wait, do not duplicate.
	existing := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-mig",
			Namespace: pinClusterNS,
			Labels:    map[string]string{labelPinnedVolumePV: pinPVLabelValue(pinPVName)},
		},
		Spec:   simplyblockv1alpha1.VolumeMigrationSpec{PVName: pinPVName, TargetNodeUUID: pinNodeA},
		Status: simplyblockv1alpha1.VolumeMigrationStatus{Phase: simplyblockv1alpha1.VolumeMigrationPhaseRunning},
	}
	api := pinAPIServer(t, []string{pinNodeA, pinNodeB}, pinNodeA)
	r, cl := newPVCReconciler(t, api, pinPVC(pinNodeB, ""), pinPV(), pinClusterCR(), existing)
	res, err := r.Reconcile(context.Background(), pinRequest())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue while a migration is in flight")
	}
	if got := len(listPinMigrations(t, cl)); got != 1 {
		t.Fatalf("expected only the pre-existing migration, got %d", got)
	}
	if _, ok := getPinPVC(t, cl).Annotations[simplyblockv1alpha1.AnnotationPinnedVolumeApplied]; ok {
		t.Fatalf("applied must not be set while waiting for an in-flight migration")
	}
}
