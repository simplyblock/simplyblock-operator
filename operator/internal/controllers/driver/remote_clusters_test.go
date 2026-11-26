// Remote storage clusters declared on the SimplyblockDriver, and the entries the
// operator writes for them into simplyblock-csi-secret-v2.

package driver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	clustercontroller "github.com/simplyblock/simplyblock-operator/internal/controllers/cluster"
)

const (
	euClusterID = "7c1e5b0a-2f4d-4c8e-9a61-0d3b5e7f1a22"
	usClusterID = "3f9a2d11-8b7c-4e05-b2d4-6a1c9e0f4b37"
)

// managementAPI answers a storage-node listing for any cluster, and refuses a
// bearer token other than secret with 401.
func managementAPI(t *testing.T, secret string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/storage-nodes/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func credentialsSecret(name, clusterSecret string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "simplyblock", Name: name},
		Data:       map[string][]byte{"cluster_secret": []byte(clusterSecret)},
	}
}

func withRemote(d *simplyblockv1alpha2.SimplyblockDriver, clusterID, endpoint, secretName string) *simplyblockv1alpha2.SimplyblockDriver {
	d.Spec.RemoteClusters = append(d.Spec.RemoteClusters, simplyblockv1alpha2.RemoteStorageCluster{
		ClusterID:            clusterID,
		Endpoint:             endpoint,
		CredentialsSecretRef: corev1.LocalObjectReference{Name: secretName},
	})
	return d
}

func csiSecretWith(entries ...clustercontroller.CSIClusterEntry) *corev1.Secret {
	payload, _ := json.Marshal(clustercontroller.CSICredentials{Clusters: entries})
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "simplyblock", Name: "simplyblock-csi-secret-v2"},
		Data:       map[string][]byte{"secret.json": payload},
	}
}

// reconcileRemote runs one reconcile and returns the client and the events it
// recorded.
func reconcileRemote(
	t *testing.T, d *simplyblockv1alpha2.SimplyblockDriver, objs ...client.Object,
) (client.Client, *events.FakeRecorder) {
	t.Helper()
	scheme := reconcilerScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, d)...).WithStatusSubresource(d).Build()
	rec := events.NewFakeRecorder(50)
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme, Snapshots: fixedSnapshotAPI{served: true}, Recorder: rec}
	if _, err := r.Reconcile(context.Background(), requestFor(d)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return c, rec
}

func readCSIEntries(t *testing.T, c client.Client) map[string]clustercontroller.CSIClusterEntry {
	t.Helper()
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: "simplyblock", Name: "simplyblock-csi-secret-v2"}
	if err := c.Get(context.Background(), key, &secret); err != nil {
		t.Fatalf("simplyblock-csi-secret-v2: %v", err)
	}
	var creds clustercontroller.CSICredentials
	if err := json.Unmarshal(secret.Data["secret.json"], &creds); err != nil {
		t.Fatalf("decode secret.json: %v", err)
	}
	out := map[string]clustercontroller.CSIClusterEntry{}
	for _, e := range creds.Clusters {
		out[e.ClusterID] = e
	}
	return out
}

func readDriver(t *testing.T, c client.Client, d *simplyblockv1alpha2.SimplyblockDriver) *simplyblockv1alpha2.SimplyblockDriver {
	t.Helper()
	var got simplyblockv1alpha2.SimplyblockDriver
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), &got); err != nil {
		t.Fatalf("read driver: %v", err)
	}
	return &got
}

func remoteState(d *simplyblockv1alpha2.SimplyblockDriver, clusterID string) simplyblockv1alpha2.RemoteStorageClusterState {
	for _, s := range d.Status.RemoteClusters {
		if s.ClusterID == clusterID {
			return s.State
		}
	}
	return ""
}

func drainEvents(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// Two remote clusters behind two management APIs get two entries, each with
// its own endpoint and secret, and both are reported Connected.
func TestRemoteClustersAreWrittenIntoTheCSISecret(t *testing.T) {
	eu := managementAPI(t, "eu-secret")
	us := managementAPI(t, "us-secret")
	d := withRemote(withRemote(testDriver("simplyblock"),
		euClusterID, eu.URL, "eu-creds"), usClusterID, us.URL, "us-creds")

	c, _ := reconcileRemote(t, d, credentialsSecret("eu-creds", "eu-secret"), credentialsSecret("us-creds", "us-secret"))

	entries := readCSIEntries(t, c)
	for id, want := range map[string][2]string{euClusterID: {eu.URL, "eu-secret"}, usClusterID: {us.URL, "us-secret"}} {
		got, ok := entries[id]
		if !ok {
			t.Fatalf("no entry for %s in %v", id, entries)
		}
		if got.ClusterEndpoint != want[0] || got.ClusterSecret != want[1] || got.Local {
			t.Errorf("entry %s = %+v, want endpoint %s, secret %s, not local", id, got, want[0], want[1])
		}
	}
	got := readDriver(t, c, d)
	if remoteState(got, euClusterID) != simplyblockv1alpha2.RemoteStorageClusterConnected ||
		remoteState(got, usClusterID) != simplyblockv1alpha2.RemoteStorageClusterConnected {
		t.Errorf("status = %+v, want both Connected", got.Status.RemoteClusters)
	}
	if got.Status.RemoteClustersTotal != 2 || got.Status.RemoteClustersConnected != 2 {
		t.Errorf("totals = %d/%d, want 2/2", got.Status.RemoteClustersConnected, got.Status.RemoteClustersTotal)
	}
}

// A refused secret is reported Unauthorized, and the entry is still written,
// so the state is visible without taking the cluster away from the driver.
func TestARefusedClusterSecretIsUnauthorizedAndKeepsItsEntry(t *testing.T) {
	eu := managementAPI(t, "eu-secret")
	d := withRemote(testDriver("simplyblock"), euClusterID, eu.URL, "eu-creds")

	c, rec := reconcileRemote(t, d, credentialsSecret("eu-creds", "wrong"))

	if _, ok := readCSIEntries(t, c)[euClusterID]; !ok {
		t.Errorf("no entry for %s", euClusterID)
	}
	got := readDriver(t, c, d)
	if s := remoteState(got, euClusterID); s != simplyblockv1alpha2.RemoteStorageClusterUnauthorized {
		t.Errorf("state = %q, want Unauthorized", s)
	}
	if got.Status.RemoteClustersConnected != 0 {
		t.Errorf("connected = %d, want 0", got.Status.RemoteClustersConnected)
	}
	if evs := strings.Join(drainEvents(rec), "\n"); !strings.Contains(evs, "RemoteClusterUnauthorized") {
		t.Errorf("events = %q, want RemoteClusterUnauthorized", evs)
	}
}

// A missing credentials Secret is CredentialsMissing, and no entry is written.
func TestAMissingCredentialsSecretWritesNoEntry(t *testing.T) {
	eu := managementAPI(t, "eu-secret")
	d := withRemote(testDriver("simplyblock"), euClusterID, eu.URL, "eu-creds")

	c, _ := reconcileRemote(t, d, csiSecretWith())

	if _, ok := readCSIEntries(t, c)[euClusterID]; ok {
		t.Errorf("an entry was written for %s without its credentials", euClusterID)
	}
	if s := remoteState(readDriver(t, c, d), euClusterID); s != simplyblockv1alpha2.RemoteStorageClusterCredentialsMissing {
		t.Errorf("state = %q, want CredentialsMissing", s)
	}
}

// An item removed from the spec has its entry removed. Local and peer entries,
// which a StorageCluster wrote, are left alone.
func TestARemovedItemRemovesOnlyItsOwnEntry(t *testing.T) {
	const localID, peerID = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	d := testDriver("simplyblock")
	d.Status.RemoteClusters = []simplyblockv1alpha2.RemoteStorageClusterStatus{
		{ClusterID: euClusterID, State: simplyblockv1alpha2.RemoteStorageClusterConnected},
	}
	secret := csiSecretWith(
		clustercontroller.CSIClusterEntry{ClusterID: euClusterID, ClusterEndpoint: "https://eu.example.com", ClusterSecret: "s"},
		clustercontroller.CSIClusterEntry{ClusterID: localID, ClusterEndpoint: "http://local", ClusterSecret: "l", Local: true},
		clustercontroller.CSIClusterEntry{ClusterID: peerID, ClusterEndpoint: "http://local", ClusterSecret: "p"},
	)

	c, rec := reconcileRemote(t, d, secret)

	entries := readCSIEntries(t, c)
	if _, ok := entries[euClusterID]; ok {
		t.Errorf("the removed cluster's entry is still there: %v", entries)
	}
	if _, ok := entries[localID]; !ok {
		t.Errorf("the local entry was removed")
	}
	if _, ok := entries[peerID]; !ok {
		t.Errorf("the peer entry was removed")
	}
	if got := readDriver(t, c, d); len(got.Status.RemoteClusters) != 0 {
		t.Errorf("status still reports %+v", got.Status.RemoteClusters)
	}
	if evs := strings.Join(drainEvents(rec), "\n"); strings.Contains(evs, "RemoteClusterRemovedWithVolumes") {
		t.Errorf("a cluster with no volumes raised %q", evs)
	}
}

// Removing a cluster that PersistentVolumes still use removes the entry and
// warns, naming the volumes.
func TestRemovingAClusterWithVolumesWarns(t *testing.T) {
	d := testDriver("simplyblock")
	d.Status.RemoteClusters = []simplyblockv1alpha2.RemoteStorageClusterStatus{
		{ClusterID: euClusterID, State: simplyblockv1alpha2.RemoteStorageClusterConnected},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-eu-1"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
			Driver:       "csi.simplyblock.io",
			VolumeHandle: euClusterID + ":pool1:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		}}},
	}
	secret := csiSecretWith(clustercontroller.CSIClusterEntry{ClusterID: euClusterID, ClusterEndpoint: "https://eu.example.com", ClusterSecret: "s"})

	c, rec := reconcileRemote(t, d, secret, pv)

	if _, ok := readCSIEntries(t, c)[euClusterID]; ok {
		t.Errorf("the entry was kept; removal is not blocked by volumes")
	}
	evs := strings.Join(drainEvents(rec), "\n")
	if !strings.Contains(evs, "RemoteClusterRemovedWithVolumes") || !strings.Contains(evs, "pvc-eu-1") {
		t.Errorf("events = %q, want RemoteClusterRemovedWithVolumes naming pvc-eu-1", evs)
	}
}

// A cluster ID a local StorageCluster manages is a Conflict, and the local
// entry is not overwritten.
func TestARemoteClusterThatIsLocalIsAConflict(t *testing.T) {
	eu := managementAPI(t, "eu-secret")
	d := withRemote(testDriver("simplyblock"), euClusterID, eu.URL, "eu-creds")
	secret := csiSecretWith(clustercontroller.CSIClusterEntry{
		ClusterID: euClusterID, ClusterEndpoint: "http://local", ClusterSecret: "local-secret", Local: true,
	})

	c, _ := reconcileRemote(t, d, secret, credentialsSecret("eu-creds", "eu-secret"))

	got := readCSIEntries(t, c)[euClusterID]
	if !got.Local || got.ClusterEndpoint != "http://local" || got.ClusterSecret != "local-secret" {
		t.Errorf("local entry overwritten: %+v", got)
	}
	if s := remoteState(readDriver(t, c, d), euClusterID); s != simplyblockv1alpha2.RemoteStorageClusterConflict {
		t.Errorf("state = %q, want Conflict", s)
	}
}

// Deleting the driver removes the entries it wrote and leaves the rest.
func TestDeletingTheDriverRemovesItsRemoteEntries(t *testing.T) {
	const localID = "11111111-1111-1111-1111-111111111111"
	d := withRemote(testDriver("simplyblock"), euClusterID, "https://eu.example.com", "eu-creds")
	d.Finalizers = []string{driverFinalizer}
	now := metav1.Now()
	d.DeletionTimestamp = &now
	d.Status.RemoteClusters = []simplyblockv1alpha2.RemoteStorageClusterStatus{
		{ClusterID: euClusterID, State: simplyblockv1alpha2.RemoteStorageClusterConnected},
	}
	secret := csiSecretWith(
		clustercontroller.CSIClusterEntry{ClusterID: euClusterID, ClusterEndpoint: "https://eu.example.com", ClusterSecret: "s"},
		clustercontroller.CSIClusterEntry{ClusterID: localID, ClusterEndpoint: "http://local", ClusterSecret: "l", Local: true},
	)

	c, _ := reconcileRemote(t, d, secret)

	entries := readCSIEntries(t, c)
	if _, ok := entries[euClusterID]; ok {
		t.Errorf("the driver's entry survived its deletion: %v", entries)
	}
	if _, ok := entries[localID]; !ok {
		t.Errorf("the local entry was removed with the driver")
	}
}
