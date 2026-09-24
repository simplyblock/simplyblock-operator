// Unit coverage for how the multi-cluster spec resolves a pool name per backend
// cluster. It lives beside the spec it covers rather than in a package of its
// own, and it is a plain Go test rather than a ginkgo one because it reads an
// environment variable and returns a slice: it needs no cluster, so requiring
// one to run it would be requiring one to check string parsing.

package e2e

import (
	"slices"
	"testing"
)

func TestPoolNamesForClusters(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		clusters int
		want     []string
		wantErr  bool
	}{{
		name:     "unset gives every cluster the default pool",
		env:      "",
		clusters: 2,
		want:     []string{"pool1", "pool1"},
	}, {
		name:     "one name gives every cluster that pool",
		env:      "shared",
		clusters: 2,
		want:     []string{"shared", "shared"},
	}, {
		// The case this helper exists for: two clusters in one namespace cannot
		// both own a StoragePool called pool1, because the CR name is the pool
		// name and two objects of one kind cannot share a name in a namespace.
		name:     "one name per cluster is taken in order",
		env:      "pool-a,pool-b",
		clusters: 2,
		want:     []string{"pool-a", "pool-b"},
	}, {
		name:     "spaces around the names are not part of them",
		env:      " pool-a , pool-b ",
		clusters: 2,
		want:     []string{"pool-a", "pool-b"},
	}, {
		name:     "a count that matches neither one nor the clusters is refused",
		env:      "pool-a,pool-b,pool-c",
		clusters: 2,
		wantErr:  true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MULTI_CLUSTER_POOL_NAME", tc.env)

			got, err := poolNamesForClusters(tc.clusters)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("expected %q against %d clusters to be refused, got %v",
					tc.env, tc.clusters, got)
			case tc.wantErr:
				return
			case err != nil:
				t.Fatalf("unexpected error for %q: %v", tc.env, err)
			}

			if !slices.Equal(got, tc.want) {
				t.Fatalf("pool names for %q: got %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}
