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

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
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
func pinClusterCR() *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: pinClusterName, Namespace: pinClusterNS},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: pinCluster},
	}
}

// pinStorageNode is the object a backend node UUID is resolved to. The
// redesigned kind names the Kubernetes object rather than the UUID, so a move
// to a node nothing reports cannot be raised at all.
func pinStorageNode(name, uuid string) *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pinClusterNS},
		Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: pinClusterName},
		Status:     simplyblockv1alpha2.StorageNodeStatus{UUID: uuid},
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
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme,
		simplyblockv1alpha2.AddToScheme, corev1.AddToScheme)
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
		ann[kube.AnnoSelectedStorageNode] = pinned
	}
	if applied != "" {
		ann[kube.AnnoSelectedStorageNodeApplied] = applied
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

// listPinMigrations reads the moves this controller raised, whichever kind
// carries them.
func listPinMigrations(t *testing.T, r *PersistentVolumeClaimReconciler) []volumemigration.Move {
	t.Helper()
	moves, err := r.mover().List(context.Background(), pinClusterNS, nil)
	if err != nil {
		t.Fatalf("list the volume moves: %v", err)
	}
	return moves
}

func TestPVCReconcile_NoChangeGate(t *testing.T) {
	// desired == applied → nothing happens, no API call.
	r, _ := newPVCReconciler(t, unreachableAPI, pinPVC(pinNodeB, pinNodeB), pinPV())
	if _, err := r.Reconcile(context.Background(), pinRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(listPinMigrations(t, r)); got != 0 {
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
	if _, ok := pvc.Annotations[kube.AnnoSelectedStorageNodeApplied]; ok {
		t.Fatalf("expected applied annotation cleared, still present")
	}
	if got := len(listPinMigrations(t, r)); got != 0 {
		t.Fatalf("expected no migrations, got %d", got)
	}
}

func TestPVCReconcile_UnboundRequeues(t *testing.T) {
	pvc := pinPVC(pinNodeB, "")
	pvc.Spec.VolumeName = ""
	r, _ := newPVCReconciler(t, unreachableAPI, pvc)
	res, err := r.Reconcile(context.Background(), pinRequest())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue for unbound PVC")
	}
	if got := len(listPinMigrations(t, r)); got != 0 {
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
	if got := len(listPinMigrations(t, r)); got != 0 {
		t.Fatalf("expected no migrations for invalid target, got %d", got)
	}
	pvc := getPinPVC(t, cl)
	if pvc.Annotations[kube.AnnoSelectedStorageNodeRejected] != "ghost-node" {
		t.Fatalf("expected rejected annotation = ghost-node, got %q",
			pvc.Annotations[kube.AnnoSelectedStorageNodeRejected])
	}
	if _, ok := pvc.Annotations[kube.AnnoSelectedStorageNodeApplied]; ok {
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
	if got := len(listPinMigrations(t, r)); got != 0 {
		t.Fatalf("expected no migrations, got %d", got)
	}
	if getPinPVC(t, cl).Annotations[kube.AnnoSelectedStorageNodeApplied] != pinNodeB {
		t.Fatalf("expected applied = %s", pinNodeB)
	}
}

func TestPVCReconcile_LegacyHostIDNormalized(t *testing.T) {
	// A pre-existing PVC pinned via the legacy host-id annotation (volume already
	// on that node) is honored as a pin and normalized: selected-storage-node is
	// written, host-id dropped, applied recorded — no migration.
	api := pinAPIServer(t, []string{pinNodeA, pinNodeB}, pinNodeB)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pinPVCName,
			Namespace:   pinNamespace,
			Annotations: map[string]string{kube.AnnoHostID: pinNodeB},
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pinPVName},
	}
	r, cl := newPVCReconciler(t, api, pvc, pinPV())
	if _, err := r.Reconcile(context.Background(), pinRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(listPinMigrations(t, r)); got != 0 {
		t.Fatalf("expected no migrations (already on node), got %d", got)
	}
	got := getPinPVC(t, cl)
	if got.Annotations[kube.AnnoSelectedStorageNode] != pinNodeB {
		t.Errorf("selected-storage-node = %q, want %s (normalized from host-id)",
			got.Annotations[kube.AnnoSelectedStorageNode], pinNodeB)
	}
	if _, ok := got.Annotations[kube.AnnoHostID]; ok {
		t.Error("legacy host-id should have been removed after normalization")
	}
	if got.Annotations[kube.AnnoSelectedStorageNodeApplied] != pinNodeB {
		t.Errorf("applied = %q, want %s", got.Annotations[kube.AnnoSelectedStorageNodeApplied], pinNodeB)
	}
}

func TestPVCReconcile_ValidChangeCreatesMigration(t *testing.T) {
	// Volume on node-a, pin to node-b → create migration + record applied.
	api := pinAPIServer(t, []string{pinNodeA, pinNodeB}, pinNodeA)
	r, cl := newPVCReconciler(t, api, pinPVC(pinNodeB, ""), pinPV(), pinClusterCR(),
		pinStorageNode("node-a", pinNodeA), pinStorageNode("node-b", pinNodeB))
	if _, err := r.Reconcile(context.Background(), pinRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	migs := listPinMigrations(t, r)
	if len(migs) != 1 {
		t.Fatalf("expected 1 move, got %d", len(migs))
	}
	if migs[0].PVName != pinPVName {
		t.Fatalf("the move names volume %q, want %q", migs[0].PVName, pinPVName)
	}

	// The move names the node object rather than the backend UUID, and is
	// attributed to the cluster that owns it rather than to the claim: a claim
	// may live in another namespace, and a cross-namespace reference is invalid.
	var raised simplyblockv1alpha2.PersistentVolumeOps
	if err := cl.Get(context.Background(),
		types.NamespacedName{Name: migs[0].Name}, &raised); err != nil {
		t.Fatalf("reading the move back: %v", err)
	}
	if raised.Spec.Migrate == nil || raised.Spec.Migrate.TargetNodeRef.Name != "node-b" {
		t.Fatalf("the move's target is %+v, want the StorageNode reporting %s",
			raised.Spec.Migrate, pinNodeB)
	}
	if raised.Labels[labelPinnedVolumePV] != pinPVLabelValue(pinPVName) {
		t.Fatalf("expected PV label %q, got %q",
			pinPVLabelValue(pinPVName), raised.Labels[labelPinnedVolumePV])
	}
	if raised.Spec.CreatorRef == nil || raised.Spec.CreatorRef.Name != pinClusterName {
		t.Fatalf("expected the cluster as the creator, got %+v", raised.Spec.CreatorRef)
	}
	if getPinPVC(t, cl).Annotations[kube.AnnoSelectedStorageNodeApplied] != pinNodeB {
		t.Fatalf("expected applied = %s after raising the move", pinNodeB)
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
	if got := len(listPinMigrations(t, r)); got != 0 {
		t.Fatalf("expected no migrations, got %d", got)
	}
	if _, ok := getPinPVC(t, cl).Annotations[kube.AnnoSelectedStorageNodeApplied]; ok {
		t.Fatalf("applied must not be set when the migration could not be created")
	}
}

func TestPVCReconcile_ActiveMigrationWaits(t *testing.T) {
	// A non-terminal migration for this PV already exists → wait, do not duplicate.
	existing := &simplyblockv1alpha2.PersistentVolumeOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "existing-move",
			Labels: map[string]string{labelPinnedVolumePV: pinPVLabelValue(pinPVName)},
		},
		Spec: simplyblockv1alpha2.PersistentVolumeOpsSpec{
			PersistentVolumeName: pinPVName,
			Action:               simplyblockv1alpha2.PersistentVolumeOpsActionMigrate,
		},
		Status: simplyblockv1alpha2.PersistentVolumeOpsStatus{
			Phase: simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning,
		},
	}
	api := pinAPIServer(t, []string{pinNodeA, pinNodeB}, pinNodeA)
	r, cl := newPVCReconciler(t, api, pinPVC(pinNodeB, ""), pinPV(), pinClusterCR(),
		pinStorageNode("node-a", pinNodeA), pinStorageNode("node-b", pinNodeB), existing)
	res, err := r.Reconcile(context.Background(), pinRequest())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue while a migration is in flight")
	}
	if got := len(listPinMigrations(t, r)); got != 1 {
		t.Fatalf("expected only the pre-existing move, got %d", got)
	}
	if _, ok := getPinPVC(t, cl).Annotations[kube.AnnoSelectedStorageNodeApplied]; ok {
		t.Fatalf("applied must not be set while waiting for an in-flight migration")
	}
}
