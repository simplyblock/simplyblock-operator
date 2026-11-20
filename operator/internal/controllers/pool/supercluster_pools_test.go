// A pool on a supercluster (design-superclusters.md §7.1): one backend pool in
// every member under the pool's name, the capacity ceiling divided among the
// members that accept new volumes, the whole allowed-node list in each, and a
// pool that is Ready once every such member holds it.

package pool

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	theSupercluster     = "prod"
	theSuperclusterUUID = "0d6f1b2e-1f0a-4c71-9a0e-5b7d2c3e4f50"
	memberA             = "prod-dense-kqv"
	memberAUUID         = "4f0c0000-0000-4000-8000-00000000000a"
	memberB             = "prod-fast-mzt"
	memberBUUID         = "9b210000-0000-4000-8000-00000000000b"
	memberC             = "prod-archive-hwd"
	memberCUUID         = "c7e80000-0000-4000-8000-00000000000c"

	// tenantAName is the pool every test here reconciles.
	tenantAName = "tenant-a"
)

// memberControlPlane is one control plane serving several clusters, answering
// each cluster's storage-pool endpoints from that cluster's own table.
type memberControlPlane struct {
	t  *testing.T
	mu sync.Mutex

	// pools is each cluster's pools by UUID.
	pools map[string]map[string]*memberPool
	// refuse makes a cluster's creates fail with the given status.
	refuse map[string]int
	// conflict makes a cluster answer a create with 409 after recording the
	// pool, as a create that landed before its answer was lost does.
	conflict map[string]bool

	posts   map[string]int
	puts    map[string][]int64
	deletes map[string]int

	server *httptest.Server
}

type memberPool struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	MaxSize      int64    `json:"max_size"`
	AllowedHosts []string `json:"allowed_hosts"`
}

func newMemberControlPlane(t *testing.T) *memberControlPlane {
	t.Helper()
	cp := &memberControlPlane{
		t:        t,
		pools:    map[string]map[string]*memberPool{},
		refuse:   map[string]int{},
		conflict: map[string]bool{},
		posts:    map[string]int{},
		puts:     map[string][]int64{},
		deletes:  map[string]int{},
	}
	cp.server = httptest.NewServer(http.HandlerFunc(cp.handle))
	t.Cleanup(cp.server.Close)
	return cp
}

func (cp *memberControlPlane) client() func() *webapi.Client {
	return func() *webapi.Client {
		return &webapi.Client{BaseURL: cp.server.URL, HttpClient: cp.server.Client()}
	}
}

// tenantA returns the cluster's pool named tenant-a, the pool every test
// here reconciles, or nil.
func (cp *memberControlPlane) tenantA(cluster string) *memberPool {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for _, p := range cp.pools[cluster] {
		if p.Name == tenantAName {
			return p
		}
	}
	return nil
}

func (cp *memberControlPlane) handle(w http.ResponseWriter, r *http.Request) {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	// /api/v2/clusters/{cluster}/storage-pools/{pool}[/host]
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 || parts[2] != "clusters" || parts[4] != "storage-pools" {
		cp.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	cluster := parts[3]
	if cp.pools[cluster] == nil {
		cp.pools[cluster] = map[string]*memberPool{}
	}
	pools := cp.pools[cluster]

	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	switch {
	case len(parts) == 5 && r.Method == http.MethodGet:
		list := make([]memberPool, 0, len(pools))
		for _, p := range pools {
			list = append(list, memberPool{ID: p.ID, Name: p.Name, Status: p.Status})
		}
		writeJSON(w, list)
	case len(parts) == 5 && r.Method == http.MethodPost:
		cp.posts[cluster]++
		if status := cp.refuse[cluster]; status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"refused"}`))
			return
		}
		name, _ := body["name"].(string)
		size, _ := body["pool_max"].(float64)
		p := &memberPool{
			ID:     fmt.Sprintf("%s-pool-%d", cluster[:4], len(pools)+1),
			Name:   name,
			Status: "active",
			// The response encodes the size as a float, so the value read back
			// is the one sent.
			MaxSize: int64(size),
		}
		pools[p.ID] = p
		if cp.conflict[cluster] {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"pool exists"}`))
			return
		}
		writeJSON(w, p)
	case len(parts) == 7 && parts[6] == "host":
		p := pools[parts[5]]
		host, _ := body["host_nqn"].(string)
		if r.Method == http.MethodPost {
			p.AllowedHosts = append(p.AllowedHosts, host)
		} else {
			kept := p.AllowedHosts[:0]
			for _, h := range p.AllowedHosts {
				if h != host {
					kept = append(kept, h)
				}
			}
			p.AllowedHosts = kept
		}
		writeJSON(w, map[string]string{})
	case len(parts) == 6 && r.Method == http.MethodGet:
		p := pools[parts[5]]
		if p == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, p)
	case len(parts) == 6 && r.Method == http.MethodPut:
		p := pools[parts[5]]
		size, _ := body["pool_max"].(float64)
		cp.puts[cluster] = append(cp.puts[cluster], int64(size))
		if size > 0 {
			p.MaxSize = int64(size)
		}
		writeJSON(w, p)
	case len(parts) == 6 && r.Method == http.MethodDelete:
		cp.deletes[cluster]++
		delete(pools, parts[5])
		w.WriteHeader(http.StatusOK)
	default:
		cp.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func aSupercluster() *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: theSupercluster, Namespace: testNamespace, UID: theSuperclusterUUID},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			Supercluster: &simplyblockv1alpha2.SuperclusterSpec{},
		},
		Status: simplyblockv1alpha2.StorageClusterStatus{UUID: theSuperclusterUUID},
	}
}

func aMember(name, uuid string, phase simplyblockv1alpha2.StorageClusterPhase) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       simplyblockv1alpha2.StorageClusterSpec{SuperclusterRef: theSupercluster},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: uuid, Phase: phase},
	}
}

func aSuperclusterPool(name string, mutate ...func(*simplyblockv1alpha2.StoragePool)) *simplyblockv1alpha2.StoragePool {
	return newPool(name, append([]func(*simplyblockv1alpha2.StoragePool){
		func(p *simplyblockv1alpha2.StoragePool) {
			p.Spec.ClusterRef = theSupercluster
			p.UID = types.UID("pool-uid-" + name)
		},
	}, mutate...)...)
}

func superclusterReconciler(
	t *testing.T, cp *memberControlPlane, rec *recorder, objects ...client.Object,
) *StoragePoolReconciler {
	t.Helper()
	return &StoragePoolReconciler{
		Client:       newClient(t, objects...),
		Scheme:       testScheme(t),
		Recorder:     rec,
		NewAPIClient: cp.client(),
	}
}

func memberRow(t *testing.T, p *simplyblockv1alpha2.StoragePool, cluster string) simplyblockv1alpha2.PoolMemberStatus {
	t.Helper()
	for _, row := range p.Status.Members {
		if row.Cluster == cluster {
			return row
		}
	}
	t.Fatalf("pool %s has no row for member %s: %+v", p.Name, cluster, p.Status.Members)
	return simplyblockv1alpha2.PoolMemberStatus{}
}

// I-21: a pool on a supercluster is created in every member under the pool's
// name, and the pool records each member's backend UUID and none of its own.
func TestAPoolOnASuperclusterIsCreatedInEveryMember(t *testing.T) {
	cp, rec := newMemberControlPlane(t), &recorder{}
	r := superclusterReconciler(t, cp, rec, aSupercluster(),
		aMember(memberA, memberAUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aMember(memberB, memberBUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aSuperclusterPool("tenant-a"))

	p, _ := reconcileSettled(t, r, "tenant-a")

	for _, member := range []struct{ name, uuid string }{{memberA, memberAUUID}, {memberB, memberBUUID}} {
		backend := cp.tenantA(member.uuid)
		if backend == nil {
			t.Fatalf("member %s has no backend pool named tenant-a", member.name)
		}
		row := memberRow(t, p, member.name)
		if row.UUID != backend.ID || row.Phase != simplyblockv1alpha2.StoragePoolPhaseReady {
			t.Errorf("member %s's row = %+v, want UUID %s and Ready", member.name, row, backend.ID)
		}
		if cp.posts[member.uuid] != 1 {
			t.Errorf("member %s was asked to create the pool %d times, want 1", member.name, cp.posts[member.uuid])
		}
	}
	if p.Status.UUID != "" {
		t.Errorf("status.uuid = %q, want empty for a pool of a supercluster", p.Status.UUID)
	}
	if p.Status.Phase != simplyblockv1alpha2.StoragePoolPhaseReady {
		t.Errorf("phase = %s (%s), want Ready", p.Status.Phase, p.Status.Message)
	}
	if !metav1.IsControlledBy(p, aSupercluster()) {
		t.Errorf("the pool is not owned by its supercluster: %+v", p.OwnerReferences)
	}
	if !rec.has(PoolCreated) {
		t.Errorf("no %s event: %+v", PoolCreated, rec.events)
	}
}

// I-22: the pool is Ready only once every member accepting volumes holds it. A
// member still being built neither holds it nor holds the pool back.
func TestThePoolIsReadyOnceEveryAcceptingMemberHoldsIt(t *testing.T) {
	cp, rec := newMemberControlPlane(t), &recorder{}
	cp.refuse[memberBUUID] = http.StatusInternalServerError
	r := superclusterReconciler(t, cp, rec, aSupercluster(),
		aMember(memberA, memberAUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aMember(memberB, memberBUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aMember(memberC, "", simplyblockv1alpha2.StorageClusterPhaseCreating),
		aSuperclusterPool("tenant-a"))

	p, _ := reconcileSettled(t, r, "tenant-a")
	if p.Status.Phase != simplyblockv1alpha2.StoragePoolPhasePending || !strings.Contains(p.Status.Message, memberB) {
		t.Errorf("phase = %s (%q), want Pending naming %s", p.Status.Phase, p.Status.Message, memberB)
	}
	if strings.Contains(p.Status.Message, memberC) {
		t.Errorf("message %q names %s, which accepts no volumes yet", p.Status.Message, memberC)
	}

	cp.refuse[memberBUUID] = 0
	p, _ = reconcileSettled(t, r, "tenant-a")
	if p.Status.Phase != simplyblockv1alpha2.StoragePoolPhaseReady {
		t.Errorf("phase = %s (%q), want Ready with %s still being built",
			p.Status.Phase, p.Status.Message, memberC)
	}
	if row := memberRow(t, p, memberC); row.Phase != simplyblockv1alpha2.StoragePoolPhasePending {
		t.Errorf("%s's row = %+v, want Pending", memberC, row)
	}
}

// I-23: a create answered with 409 is a pool that exists, and it is adopted
// rather than created again.
func TestAConflictOnCreateAdoptsTheExistingPool(t *testing.T) {
	cp, rec := newMemberControlPlane(t), &recorder{}
	cp.conflict[memberAUUID] = true
	r := superclusterReconciler(t, cp, rec, aSupercluster(),
		aMember(memberA, memberAUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aSuperclusterPool("tenant-a"))

	p, _ := reconcileSettled(t, r, "tenant-a")

	if row := memberRow(t, p, memberA); row.UUID != cp.tenantA(memberAUUID).ID {
		t.Errorf("%s's row = %+v, want the existing pool adopted", memberA, row)
	}
	if cp.posts[memberAUUID] != 1 {
		t.Errorf("creates sent = %d, want 1", cp.posts[memberAUUID])
	}
}

// I-24: one member's create failing leaves that member's row Pending with the
// refusal, and the pool's status names the member.
func TestAFailingMemberIsNamedInThePoolsStatus(t *testing.T) {
	cp, rec := newMemberControlPlane(t), &recorder{}
	cp.refuse[memberBUUID] = http.StatusBadRequest
	r := superclusterReconciler(t, cp, rec, aSupercluster(),
		aMember(memberA, memberAUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aMember(memberB, memberBUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aSuperclusterPool("tenant-a"))

	p, result := reconcileSettled(t, r, "tenant-a")

	row := memberRow(t, p, memberB)
	if row.Phase != simplyblockv1alpha2.StoragePoolPhasePending || row.UUID != "" || !strings.Contains(row.Message, "refused") {
		t.Errorf("%s's row = %+v, want Pending with the refusal", memberB, row)
	}
	if memberRow(t, p, memberA).Phase != simplyblockv1alpha2.StoragePoolPhaseReady {
		t.Errorf("%s's row is not Ready: %+v", memberA, memberRow(t, p, memberA))
	}
	if !rec.has(PoolCreationFailed) {
		t.Errorf("no %s event: %+v", PoolCreationFailed, rec.events)
	}
	if result.RequeueAfter == 0 {
		t.Error("a refused create is not retried")
	}
}

// I-25: every member's backend pool admits every allowed node, a consuming
// node that runs no storage node included.
func TestEveryMemberAdmitsTheWholeAllowedNodeList(t *testing.T) {
	cp, rec := newMemberControlPlane(t), &recorder{}
	storage := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", UID: "uid-worker-1"}}
	consumer := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "app-1", UID: "uid-app-1"}}
	r := superclusterReconciler(t, cp, rec, aSupercluster(), storage, consumer,
		aMember(memberA, memberAUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aMember(memberB, memberBUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aSuperclusterPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
			p.Spec.AllowedNodes = []string{"worker-1", "app-1"}
			p.Spec.VolumeDefaults = &simplyblockv1alpha2.VolumeDefaults{EnableDHCHAP: ptr.To(true)}
		}))

	p, _ := reconcileSettled(t, r, "tenant-a")

	want := []string{nqn.Host("uid-app-1"), nqn.Host("uid-worker-1")}
	sort.Strings(want)
	for _, member := range []string{memberAUUID, memberBUUID} {
		got := append([]string(nil), cp.tenantA(member).AllowedHosts...)
		sort.Strings(got)
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("member %s's allowed hosts (-want +got):\n%s", member, diff)
		}
	}

	// One label per pool, whatever the member, because one class reaches the
	// pool in every member.
	var node corev1.Node
	if err := r.Get(t.Context(), client.ObjectKey{Name: "app-1"}, &node); err != nil {
		t.Fatalf("read the node: %v", err)
	}
	if node.Labels[kube.PoolNodeLabelKey(string(p.UID))] != kube.LabelPoolAllowed {
		t.Errorf("app-1 labels = %v, want the pool's label", node.Labels)
	}
}

// I-27: a member added later receives every pool of its supercluster.
func TestAMemberAddedLaterReceivesThePool(t *testing.T) {
	cp, rec := newMemberControlPlane(t), &recorder{}
	r := superclusterReconciler(t, cp, rec, aSupercluster(),
		aMember(memberA, memberAUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aSuperclusterPool("tenant-a"))
	reconcileSettled(t, r, "tenant-a")

	if err := r.Create(t.Context(), aMember(memberB, memberBUUID, simplyblockv1alpha2.StorageClusterPhaseActivating)); err != nil {
		t.Fatalf("add a member: %v", err)
	}
	p, _ := reconcileSettled(t, r, "tenant-a")

	if cp.tenantA(memberBUUID) == nil {
		t.Fatal("the member added later has no backend pool")
	}
	if row := memberRow(t, p, memberB); row.Phase != simplyblockv1alpha2.StoragePoolPhaseReady {
		t.Errorf("%s's row = %+v, want Ready", memberB, row)
	}
	if got := poolsForMember(t.Context(), r.Client, aMember(memberB, memberBUUID, "")); len(got) != 1 ||
		got[0].Name != "tenant-a" {
		t.Errorf("a member maps to pools %v, want tenant-a", got)
	}
}

// I-51, the capacity half: each member's backend pool is held to its share of
// the ceiling, and a member leaving placement has the others' shares rewritten.
func TestEachMemberIsHeldToItsShareOfTheCeiling(t *testing.T) {
	cp, rec := newMemberControlPlane(t), &recorder{}
	r := superclusterReconciler(t, cp, rec, aSupercluster(),
		aMember(memberA, memberAUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aMember(memberB, memberBUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aMember(memberC, memberCUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aSuperclusterPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
			p.Spec.Limits = &simplyblockv1alpha2.PoolLimits{Capacity: "300G"}
		}))

	p, _ := reconcileSettled(t, r, "tenant-a")
	for _, member := range []string{memberAUUID, memberBUUID, memberCUUID} {
		if got := cp.tenantA(member).MaxSize; got != 100_000_000_000 {
			t.Errorf("member %s's pool is held to %d, want 100G", member, got)
		}
	}
	if got := memberRow(t, p, memberA).Limits; got == nil || got.Capacity != "100G" {
		t.Errorf("%s's applied limits = %+v, want capacity 100G", memberA, got)
	}

	var leaving simplyblockv1alpha2.StorageCluster
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: memberC}, &leaving); err != nil {
		t.Fatal(err)
	}
	leaving.Spec.DisablePlacement = ptr.To(true)
	if err := r.Update(t.Context(), &leaving); err != nil {
		t.Fatal(err)
	}
	reconcileSettled(t, r, "tenant-a")

	for _, member := range []string{memberAUUID, memberBUUID} {
		if got := cp.tenantA(member).MaxSize; got != 150_000_000_000 {
			t.Errorf("member %s's pool is held to %d after %s left placement, want 150G", member, got, memberC)
		}
	}
	if got := cp.tenantA(memberCUUID).MaxSize; got != 100_000_000_000 {
		t.Errorf("the member out of placement is held to %d, want the 100G it had", got)
	}
}

// A settled pool writes no share again.
func TestASettledPoolWritesNoShareAgain(t *testing.T) {
	cp, rec := newMemberControlPlane(t), &recorder{}
	r := superclusterReconciler(t, cp, rec, aSupercluster(),
		aMember(memberA, memberAUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aMember(memberB, memberBUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aSuperclusterPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
			p.Spec.Limits = &simplyblockv1alpha2.PoolLimits{Capacity: "200G"}
		}))
	reconcileSettled(t, r, "tenant-a")
	before := len(cp.puts[memberAUUID]) + len(cp.puts[memberBUUID])

	reconcileSettled(t, r, "tenant-a")

	if after := len(cp.puts[memberAUUID]) + len(cp.puts[memberBUUID]); after != before {
		t.Errorf("updates sent by a settled pool = %d, want none", after-before)
	}
}

// I-47: the class written for a supercluster's default pool names the
// supercluster in cluster_id.
func TestTheSuperclustersDefaultClassNamesTheSupercluster(t *testing.T) {
	cp, rec := newMemberControlPlane(t), &recorder{}
	name := DefaultPoolName(theSupercluster)
	r := superclusterReconciler(t, cp, rec, aSupercluster(),
		aMember(memberA, memberAUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aSuperclusterPool(name, func(p *simplyblockv1alpha2.StoragePool) {
			p.Spec.VolumeDefaults = &simplyblockv1alpha2.VolumeDefaults{}
		}))

	reconcileSettled(t, r, name)

	var class storagev1.StorageClass
	if err := r.Get(t.Context(), client.ObjectKey{Name: DefaultStorageClassName(testNamespace, theSupercluster)}, &class); err != nil {
		t.Fatalf("read the default class: %v", err)
	}
	if got := class.Parameters[kube.ParamClusterID]; got != theSuperclusterUUID {
		t.Errorf("cluster_id = %q, want the supercluster's %q", got, theSuperclusterUUID)
	}
	if got := class.Parameters[kube.ParamPool]; got != name {
		t.Errorf("pool_name = %q, want %q", got, name)
	}
}

// Deleting a pool of a supercluster deletes its backend pool in every member.
func TestDeletingASuperclusterPoolDeletesItInEveryMember(t *testing.T) {
	cp, rec := newMemberControlPlane(t), &recorder{}
	r := superclusterReconciler(t, cp, rec, aSupercluster(),
		aMember(memberA, memberAUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aMember(memberB, memberBUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aSuperclusterPool("tenant-a"))
	p, _ := reconcileSettled(t, r, "tenant-a")

	if err := r.Delete(t.Context(), p); err != nil {
		t.Fatalf("delete the pool: %v", err)
	}
	gone, _ := reconcileSettled(t, r, "tenant-a")

	if gone != nil {
		t.Fatalf("the pool is still there: %+v", gone.Status)
	}
	for _, member := range []string{memberAUUID, memberBUUID} {
		if cp.deletes[member] != 1 || cp.tenantA(member) != nil {
			t.Errorf("member %s: deletes = %d, pool left = %v", member, cp.deletes[member], cp.tenantA(member))
		}
	}
}

// A bound volume in any member holds the deletion, matched by that member's
// backend UUID in the handle.
func TestAVolumeBoundInAMemberHoldsTheDeletion(t *testing.T) {
	cp, rec := newMemberControlPlane(t), &recorder{}
	r := superclusterReconciler(t, cp, rec, aSupercluster(),
		aMember(memberA, memberAUUID, simplyblockv1alpha2.StorageClusterPhaseOnline),
		aSuperclusterPool("tenant-a"))
	p, _ := reconcileSettled(t, r, "tenant-a")
	row := memberRow(t, p, memberA)

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{
				Driver:       kube.DriverName,
				VolumeHandle: memberAUUID + ":" + row.UUID + ":5e1a0000-0000-4000-8000-000000000001",
			},
		}},
	}
	if err := r.Create(t.Context(), pv); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	held, _ := reconcileSettled(t, r, "tenant-a")

	if held == nil || cp.deletes[memberAUUID] != 0 {
		t.Fatalf("the deletion was not held by a bound volume: pool=%v deletes=%d", held != nil, cp.deletes[memberAUUID])
	}
	if !rec.has(VolumesStillBound) {
		t.Errorf("no %s event: %+v", VolumesStillBound, rec.events)
	}
}
