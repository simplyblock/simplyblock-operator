package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/cluster"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// The control-plane split: the pair's target cluster is managed by another
// control plane, named by spec.peer.

const peerTargetUUID = "0b8e6c2e-3a51-4c3e-9d6f-2f7a1c0d9e11"

func peerPair() *simplyblockv1alpha1.ReplicationPair {
	p := newSitePair()
	p.Spec.TargetCluster = peerTargetUUID
	p.Spec.Peer = &simplyblockv1alpha1.ReplicationPeer{
		Endpoint:             "http://10.70.3.21:15000/",
		CredentialsSecretRef: "peer-site-b",
	}
	return p
}

func peerSecret(data map[string]string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "peer-site-b", Namespace: "default"},
		Data:       map[string][]byte{},
	}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func csiEntries(t *testing.T, cl client.Client) map[string]cluster.CSIClusterEntry {
	t.Helper()
	var s corev1.Secret
	err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "simplyblock-csi-secret-v2"}, &s)
	if apierrors.IsNotFound(err) {
		return map[string]cluster.CSIClusterEntry{}
	}
	if err != nil {
		t.Fatalf("get CSI secret: %v", err)
	}
	var creds cluster.CSICredentials
	if err := json.Unmarshal(s.Data["secret.json"], &creds); err != nil {
		t.Fatalf("unmarshal CSI secret: %v", err)
	}
	out := map[string]cluster.CSIClusterEntry{}
	for _, e := range creds.Clusters {
		out[e.ClusterID] = e
	}
	return out
}

func TestSitePair_PeerTargetCarriesThePeerAndRegistersItWithCSI(t *testing.T) {
	var posted map[string]any
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case http.MethodPost:
			_ = json.NewDecoder(req.Body).Decode(&posted)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"tgt-backend-uuid"}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSitePairReconciler(t, testCluster("default", "cluster1", "src-uuid"), peerPair(),
		peerSecret(map[string]string{"secret": "sb-secret\n", "ca.crt": "PEM"}))

	if _, err := r.Reconcile(context.Background(), sitePairRequest("pair1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if posted["target_cluster_id"] != peerTargetUUID || posted["peer_endpoint"] != "http://10.70.3.21:15000" ||
		posted["peer_secret"] != "sb-secret" || posted["peer_ca_cert"] != "PEM" {
		t.Fatalf("create-target body %v", posted)
	}
	if got := getSitePair(t, cl); !got.Status.Ready || got.Status.BackendTargetID != "tgt-backend-uuid" {
		t.Fatalf("status %+v", got.Status)
	}
	e, ok := csiEntries(t, cl)[peerTargetUUID]
	if !ok || !e.Peer || e.Local || e.ClusterEndpoint != "http://10.70.3.21:15000" || e.ClusterSecret != "sb-secret" {
		t.Fatalf("CSI entry %+v (present %v)", e, ok)
	}
}

func TestSitePair_WithoutPeerSendsNoPeerFields(t *testing.T) {
	var posted map[string]any
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_ = json.NewDecoder(req.Body).Decode(&posted)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"tgt-backend-uuid"}`))
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSitePairReconciler(t, testCluster("default", "cluster1", "src-uuid"),
		testCluster("default", "cluster2", "tgt-uuid"), newSitePair())

	if _, err := r.Reconcile(context.Background(), sitePairRequest("pair1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, k := range []string{"peer_endpoint", "peer_secret", "peer_ca_cert"} {
		if _, ok := posted[k]; ok {
			t.Errorf("create-target body has %s: %v", k, posted)
		}
	}
	if len(csiEntries(t, cl)) != 0 {
		t.Error("a pair without a peer wrote a CSI entry")
	}
}

func TestSitePair_PeerWithoutItsSecretIsNotReady(t *testing.T) {
	calls := 0
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) })
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSitePairReconciler(t, testCluster("default", "cluster1", "src-uuid"), peerPair())

	res, err := r.Reconcile(context.Background(), sitePairRequest("pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != replPairRequeueError {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replPairRequeueError)
	}
	got := getSitePair(t, cl)
	if got.Status.Ready || !strings.Contains(got.Status.Message, "peer control plane") {
		t.Fatalf("status %+v", got.Status)
	}
	if calls != 0 {
		t.Errorf("the control plane was called %d times without the peer's credentials", calls)
	}
}

func TestSitePair_DeletingAPeerPairDropsItsCSIEntryOnly(t *testing.T) {
	now := metav1.Now()
	pair := peerPair()
	pair.DeletionTimestamp = &now
	pair.Finalizers = []string{utils.FinalizerReplicationPair}
	pair.Status.BackendTargetID = "tgt-id"
	creds, _ := json.Marshal(cluster.CSICredentials{Clusters: []cluster.CSIClusterEntry{
		{ClusterID: "src-uuid", ClusterEndpoint: "http://cp:5000", ClusterSecret: "sa", Local: true},
		{ClusterID: peerTargetUUID, ClusterEndpoint: "http://10.70.3.21:15000", ClusterSecret: "sb", Peer: true},
	}})
	csi := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-csi-secret-v2", Namespace: "default"},
		Data:       map[string][]byte{"secret.json": creds},
	}
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSitePairReconciler(t, testCluster("default", "cluster1", "src-uuid"), pair, csi)

	if _, err := r.Reconcile(context.Background(), sitePairRequest("pair1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := csiEntries(t, cl)
	if _, ok := got[peerTargetUUID]; ok {
		t.Error("the peer's CSI entry outlived its pair")
	}
	if !got["src-uuid"].Local {
		t.Errorf("the local entry was touched: %+v", got)
	}
}
