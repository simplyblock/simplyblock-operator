// A StorageSiteDeployment or StretchedCluster written in YAML without its
// discover block must still take the controller's typed updates. The Go type
// holds discover as a plain struct, so every typed write sends discover: {},
// and the immutability rule must not read that as a new discovery.

package bases

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// createWithoutDiscover creates obj from a manifest that has no discover
// block anywhere, then reads it back into typed.
func createWithoutDiscover(t *testing.T, kind string, spec map[string]any, typed client.Object) {
	t.Helper()
	c := ruleServer(t)
	ctx := context.Background()
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": simplyblockv1alpha2.GroupVersion.String(),
		"kind":       kind,
		"metadata":   map[string]any{"generateName": "nodiscover-", "namespace": "default"},
		"spec":       spec,
	}}
	if err := c.Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(u), typed); err != nil {
		t.Fatalf("read back: %v", err)
	}
}

// addFinalizer is the typed update the controller makes first.
func addFinalizer(t *testing.T, obj client.Object) {
	t.Helper()
	obj.SetFinalizers(append(obj.GetFinalizers(), "storage.simplyblock.io/test"))
	if err := ruleServer(t).Update(context.Background(), obj); err != nil {
		t.Fatalf("typed update of an object created without discover refused: %v", err)
	}
}

func TestSiteDeploymentWithoutDiscoverTakesATypedUpdate(t *testing.T) {
	d := &simplyblockv1alpha2.StorageSiteDeployment{}
	createWithoutDiscover(t, "StorageSiteDeployment", map[string]any{"cluster": "site-a"}, d)
	addFinalizer(t, d)
}

func TestStretchedClusterWithoutDiscoverTakesATypedUpdate(t *testing.T) {
	c := &simplyblockv1alpha2.StretchedCluster{}
	createWithoutDiscover(t, "StretchedCluster", map[string]any{
		"clusterName": "stretched",
		"sites": []any{
			map[string]any{"cluster": "site-a", "site": "a", "role": "Leader"},
			map[string]any{"cluster": "site-b", "site": "b", "role": "Follower"},
		},
	}, c)
	addFinalizer(t, c)
}
