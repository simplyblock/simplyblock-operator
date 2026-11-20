package autoplacement

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	atlaskube "github.com/simplyblock/atlas/kube"
)

func boundPV(name, handle, claim string) *corev1.PersistentVolume {
	pv := csiPV(name, handle, "sc")
	pv.Spec.ClaimRef = &corev1.ObjectReference{Name: claim, Namespace: "app"}
	return pv
}

func claim(name string, labels, annotations map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "app", Labels: labels, Annotations: annotations,
	}}
}

// A consistency group's members live on one node/LVS: the rebalancer moving
// one of them alone would split the group, so a member is treated like a
// pinned volume and skipped.
func TestBuildPinnedSetSkipsConsistencyGroupMembers(t *testing.T) {
	objects := []client.Object{
		boundPV("pv-member", "cluster-a:pool:vol-member", "member"),
		boundPV("pv-plain", "cluster-a:pool:vol-plain", "plain"),
		boundPV("pv-pinned", "cluster-a:pool:vol-pinned", "pinned"),
		claim("member", map[string]string{consistencyGroupLabel: "db"}, nil),
		claim("plain", nil, nil),
		claim("pinned", nil, map[string]string{atlaskube.AnnoSelectedStorageNode: "node-1"}),
	}
	cl := fake.NewClientBuilder().WithScheme(namespacedTestScheme(t)).WithObjects(objects...).Build()
	lvs := NewLogicalVolumeSelector(nil, cl, nil)

	got, err := lvs.BuildPinnedSet(context.Background(), "cluster-a")
	if err != nil {
		t.Fatalf("BuildPinnedSet: %v", err)
	}
	if !got["vol-member"] {
		t.Error("a consistency-group member must be skipped by the rebalancer")
	}
	if !got["vol-pinned"] {
		t.Error("a pinned volume must stay skipped")
	}
	if got["vol-plain"] {
		t.Error("an ordinary volume must stay a rebalancing candidate")
	}
}
