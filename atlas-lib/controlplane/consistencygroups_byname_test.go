package controlplane

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A group is resolved by its name, which is the claim label it was formed
// under, and its current member count comes with it.
func TestConsistencyGroupNamedAsksByName(t *testing.T) {
	const group = "c9c9c9c9-c9c9-4c9c-8c9c-c9c9c9c9c9c9"
	var asked string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/consistency-groups/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		asked = r.URL.Query().Get("name")
		_, _ = w.Write([]byte(`[{"id":"` + group + `","cluster_id":"` + testCluster +
			`","name":"orders","member_count":3,"last_group_seq":4}]`))
	})

	got, found, err := c.ConsistencyGroupNamed(context.Background(), testCluster, "orders")

	if err != nil || !found {
		t.Fatalf("ConsistencyGroupNamed = %+v, %v, %v", got, found, err)
	}
	if asked != "orders" {
		t.Errorf("the control plane was asked for name %q, want orders", asked)
	}
	if got.ID != group || got.MemberCount != 3 {
		t.Errorf("group = %+v, want id %s with 3 members", got, group)
	}
}

// A name no group carries is not found, and not an error.
func TestConsistencyGroupNamedOfAnUnknownName(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})

	_, found, err := c.ConsistencyGroupNamed(context.Background(), testCluster, "orders")

	if err != nil || found {
		t.Errorf("an unknown name = found %v, %v; want not found and no error", found, err)
	}
}
