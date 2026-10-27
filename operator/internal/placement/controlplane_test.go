// The placement service's questions to a member's control plane: whether a
// pool holds a volume of a name, and how many volumes a consistency group has,
// each asked as that member.

package placement

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/controlplane"
)

const lookupPool = "7a7a7a7a-7a7a-4a7a-8a7a-7a7a7a7a7a7a"

// memberControlPlane answers the two lookups and remembers the token it was
// asked with.
func memberControlPlane(t *testing.T, auth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/storage-pools/"+lookupPool+"/volumes/"):
			_, _ = w.Write([]byte(`[{"id":"5e1a0000-0000-4000-8000-000000000001","name":"pvc-1","pool_name":"tenant-a",` +
				`"size":1073741824,"ns_id":1,"nqn":"nqn.2023-02.io.simplyblock:x"}]`))
		case strings.HasSuffix(r.URL.Path, "/consistency-groups/"):
			if r.URL.Query().Get("name") != "orders" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":"c9c9c9c9-c9c9-4c9c-8c9c-c9c9c9c9c9c9","cluster_id":"` + uuidA +
				`","name":"orders","member_count":2,"last_group_seq":1}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func lookupFor(t *testing.T, endpoint string) ControlPlaneLookup {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-cluster-" + memberA, Namespace: testNamespace},
		Data:       map[string][]byte{"secret": []byte("member-secret")},
	}
	return ControlPlaneLookup{
		Reader: fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(secret).Build(),
		Config: controlplane.Config{Endpoint: endpoint, Token: "operator-token"},
	}
}

// A volume is found by its name in the member's pool, asked as the member.
func TestALookupFindsAVolumeByNameAsTheMember(t *testing.T) {
	var auth string
	lookup := lookupFor(t, memberControlPlane(t, &auth).URL)

	found, err := lookup.VolumeExists(context.Background(), memberCluster(memberA, uuidA), lookupPool, "pvc-1")
	if err != nil || !found {
		t.Fatalf("VolumeExists(pvc-1) = %v, %v; want found", found, err)
	}
	if auth != "Bearer member-secret" {
		t.Errorf("asked with %q, want the member's own secret", auth)
	}
	if found, _ := lookup.VolumeExists(context.Background(), memberCluster(memberA, uuidA), lookupPool, "pvc-2"); found {
		t.Error("a name the pool does not hold was found")
	}
}

// A member without a secret of its own is asked with the operator's token.
func TestAMemberWithoutASecretIsAskedAsTheOperator(t *testing.T) {
	var auth string
	lookup := lookupFor(t, memberControlPlane(t, &auth).URL)

	if _, err := lookup.VolumeExists(context.Background(), memberCluster(memberB, uuidB), lookupPool, "pvc-1"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer operator-token" {
		t.Errorf("asked with %q, want the operator's token", auth)
	}
}

// A group's volume count comes from its name, and a name no group carries
// counts none.
func TestALookupCountsAGroupsVolumes(t *testing.T) {
	var auth string
	lookup := lookupFor(t, memberControlPlane(t, &auth).URL)

	if n, err := lookup.GroupMembers(context.Background(), memberCluster(memberA, uuidA), "orders"); err != nil || n != 2 {
		t.Errorf("GroupMembers(orders) = %d, %v; want 2", n, err)
	}
	if n, err := lookup.GroupMembers(context.Background(), memberCluster(memberA, uuidA), "billing"); err != nil || n != 0 {
		t.Errorf("GroupMembers(billing) = %d, %v; want 0", n, err)
	}
}
