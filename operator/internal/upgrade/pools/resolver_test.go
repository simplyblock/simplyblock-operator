// What the resolver has to get right is which credentials it uses, since an
// installation holds several clusters and each has its own. A resolver that
// asked the wrong cluster would answer with a pool UUID from somewhere else,
// which is worse than answering nothing: the handle would look normalized and
// name a pool the volume is not in.

package pools

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/errs"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

const (
	clusterUUID = "2f4f0300-9993-4289-be95-59414fc8a54d"
	poolUUID    = "1c2c0300-9993-4289-be95-59414fc8a54d"
	otherUUID   = "3d3d0300-9993-4289-be95-59414fc8a54d"

	// clusterName is what every fixture calls its StorageCluster, in whichever
	// namespace: a handle names its cluster by UUID, so the name is exactly the
	// thing that must not decide which credentials are used.
	clusterName = "prod"
)

// pools is a control plane answering one cluster's pool list, and recording the
// bearer token it was asked with.
func poolServer(t *testing.T, named string) (*httptest.Server, *string) {
	t.Helper()

	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		body := []map[string]any{}
		if named != "" {
			body = append(body, map[string]any{
				"id": poolUUID, "cluster_id": clusterUUID, "name": named, "max_size": 0,
			})
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func world(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering core: %v", err)
	}
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering v1alpha1: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

// storageCluster is one cluster. Both tenants call theirs prod, because the
// point of every case here is that the name is not what a handle names.
func storageCluster(namespace, uuid string) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: clusterName},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: uuid},
	}
}

func clusterSecret(namespace, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "simplyblock-cluster-" + clusterName},
		Data:       map[string][]byte{"secret": []byte(token)},
	}
}

func TestPoolUUIDResolvesThroughTheNamedClustersOwnCredentials(t *testing.T) {
	srv, seen := poolServer(t, "production")
	resolver := &Resolver{
		Client:   world(t, storageCluster("tenant-a", clusterUUID), clusterSecret("tenant-a", "tok-a")),
		Endpoint: srv.URL,
	}

	got, err := resolver.PoolUUID(t.Context(), clusterUUID, "production")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if got != poolUUID {
		t.Errorf("PoolUUID = %q, want %q", got, poolUUID)
	}
	if *seen != "Bearer tok-a" {
		t.Errorf("asked with %q, want the named cluster's own secret", *seen)
	}
}

// A second cluster in a second namespace must not supply the credentials, and
// the handle names its cluster by UUID rather than by name, so that is what
// decides.
func TestPoolUUIDIgnoresAClusterTheHandleDoesNotName(t *testing.T) {
	srv, seen := poolServer(t, "production")
	resolver := &Resolver{
		Client: world(t,
			storageCluster("tenant-a", otherUUID), clusterSecret("tenant-a", "tok-a"),
			storageCluster("tenant-b", clusterUUID), clusterSecret("tenant-b", "tok-b"),
		),
		Endpoint: srv.URL,
	}

	if _, err := resolver.PoolUUID(t.Context(), clusterUUID, "production"); err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if *seen != "Bearer tok-b" {
		t.Errorf("asked with %q, want the secret of the cluster the handle names", *seen)
	}
}

// A pool nobody answers to is a finding rather than a failure, and the caller
// tells the two apart by the wrapped error.
func TestPoolUUIDReportsANameNoPoolAnswersTo(t *testing.T) {
	srv, _ := poolServer(t, "")
	resolver := &Resolver{
		Client:   world(t, storageCluster("tenant-a", clusterUUID), clusterSecret("tenant-a", "tok-a")),
		Endpoint: srv.URL,
	}

	_, err := resolver.PoolUUID(t.Context(), clusterUUID, "vanished")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("err = %v, want it to wrap ErrNotFound", err)
	}
}

// A cluster no object reports is its own finding, and it is a different one: the
// handle names a cluster this installation does not hold, so there are no
// credentials to ask with rather than no pool to find.
func TestPoolUUIDReportsAClusterNoObjectReports(t *testing.T) {
	srv, _ := poolServer(t, "production")
	resolver := &Resolver{Client: world(t), Endpoint: srv.URL}

	_, err := resolver.PoolUUID(t.Context(), clusterUUID, "production")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("err = %v, want it to wrap ErrNotFound", err)
	}
	if err == nil || !strings.Contains(err.Error(), clusterUUID) {
		t.Errorf("the error does not name the cluster nobody reports: %v", err)
	}
}

// One answer per cluster. A cluster of a thousand volumes would otherwise list
// every pool a thousand times, and the list is the same list each time.
func TestPoolUUIDListsEachClusterOnce(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": poolUUID, "cluster_id": clusterUUID, "name": "production", "max_size": 0},
		})
	}))
	t.Cleanup(srv.Close)

	resolver := &Resolver{
		Client:   world(t, storageCluster("tenant-a", clusterUUID), clusterSecret("tenant-a", "tok-a")),
		Endpoint: srv.URL,
	}
	for range 3 {
		if _, err := resolver.PoolUUID(t.Context(), clusterUUID, "production"); err != nil {
			t.Fatalf("resolving: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("listed the pools %d times, want once for the cluster", calls)
	}
}
