package webhook

import (
	"context"
	"net/http"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	evictionNamespace = "simplyblock"
	evictionCluster   = "simplyblock-fdb-cluster"
)

func fdbPod(name, processGroup string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: evictionNamespace,
		Labels: map[string]string{
			fdbClusterNameLabel:    evictionCluster,
			fdbProcessGroupIDLabel: processGroup,
		},
	}}
}

// fdbCluster is the FoundationDBCluster with the process groups toRemove in
// its spec and present in its status. An ID ending in "+excluded" is present
// and fully excluded.
func fdbCluster(toRemove []string, present ...string) *unstructured.Unstructured {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(foundationDBClusterGVK)
	cluster.SetNamespace(evictionNamespace)
	cluster.SetName(evictionCluster)
	if toRemove != nil {
		_ = unstructured.SetNestedStringSlice(cluster.Object, toRemove, "spec", "processGroupsToRemove")
	}
	groups := make([]any, 0, len(present))
	for _, id := range present {
		group := map[string]any{"processGroupID": strings.TrimSuffix(id, "+excluded")}
		if strings.HasSuffix(id, "+excluded") {
			group["exclusionTimestamp"] = "2026-11-05T07:48:05Z"
		}
		groups = append(groups, group)
	}
	_ = unstructured.SetNestedSlice(cluster.Object, groups, "status", "processGroups")
	return cluster
}

func evictionHandler(t *testing.T, objs ...client.Object) (*FoundationDBEvictionHandler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &FoundationDBEvictionHandler{Client: c}, c
}

func evict(t *testing.T, h *FoundationDBEvictionHandler, pod string, dryRun bool) admission.Response {
	t.Helper()
	return h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Name: pod, Namespace: evictionNamespace, Operation: admissionv1.Create,
		SubResource: "eviction", DryRun: ptr.To(dryRun),
	}})
}

func toRemoveOf(t *testing.T, c client.Client) []string {
	t.Helper()
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(foundationDBClusterGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: evictionNamespace, Name: evictionCluster}, cluster); err != nil {
		t.Fatal(err)
	}
	ids, _, _ := unstructured.NestedStringSlice(cluster.Object, "spec", "processGroupsToRemove")
	return ids
}

func wantRetry(t *testing.T, resp admission.Response) {
	t.Helper()
	if resp.Allowed || resp.Result == nil || resp.Result.Code != http.StatusTooManyRequests {
		t.Fatalf("got %+v, want the eviction refused with 429 so it is retried", resp.AdmissionResponse)
	}
}

// The first eviction of a FoundationDB pod is refused and starts its
// replacement.
func TestEvictingAFoundationDBPodStartsItsReplacement(t *testing.T) {
	h, c := evictionHandler(t, fdbPod("simplyblock-fdb-cluster-log-1", "log-1"), fdbCluster(nil, "log-1", "log-2"))

	wantRetry(t, evict(t, h, "simplyblock-fdb-cluster-log-1", false))
	if got := toRemoveOf(t, c); len(got) != 1 || got[0] != "log-1" {
		t.Errorf("processGroupsToRemove = %v, want [log-1]", got)
	}
}

// Retries while the replacement runs are refused and change nothing.
func TestEvictionIsRefusedWhileTheReplacementRuns(t *testing.T) {
	h, c := evictionHandler(t, fdbPod("simplyblock-fdb-cluster-log-1", "log-1"), fdbCluster([]string{"log-1"}, "log-1"))

	wantRetry(t, evict(t, h, "simplyblock-fdb-cluster-log-1", false))
	if got := toRemoveOf(t, c); len(got) != 1 {
		t.Errorf("processGroupsToRemove = %v, want it unchanged", got)
	}
}

// A second process group waits until the first is gone: one replacement at a
// time.
func TestASecondEvictionWaitsForTheFirstReplacement(t *testing.T) {
	h, c := evictionHandler(t, fdbPod("simplyblock-fdb-cluster-storage-1", "storage-1"),
		fdbCluster([]string{"log-1"}, "log-1", "storage-1"))

	wantRetry(t, evict(t, h, "simplyblock-fdb-cluster-storage-1", false))
	if got := toRemoveOf(t, c); len(got) != 1 || got[0] != "log-1" {
		t.Errorf("processGroupsToRemove = %v, want [log-1] only", got)
	}
}

// A finished replacement no longer blocks the next one, and is dropped from
// the list.
func TestAFinishedReplacementIsDroppedFromTheList(t *testing.T) {
	h, c := evictionHandler(t, fdbPod("simplyblock-fdb-cluster-storage-1", "storage-1"),
		fdbCluster([]string{"log-1"}, "storage-1", "log-9"))

	wantRetry(t, evict(t, h, "simplyblock-fdb-cluster-storage-1", false))
	if got := toRemoveOf(t, c); len(got) != 1 || got[0] != "storage-1" {
		t.Errorf("processGroupsToRemove = %v, want [storage-1]", got)
	}
}

// A dry run changes nothing.
func TestADryRunEvictionStartsNothing(t *testing.T) {
	h, c := evictionHandler(t, fdbPod("simplyblock-fdb-cluster-log-1", "log-1"), fdbCluster(nil, "log-1"))

	wantRetry(t, evict(t, h, "simplyblock-fdb-cluster-log-1", true))
	if got := toRemoveOf(t, c); len(got) != 0 {
		t.Errorf("a dry run set processGroupsToRemove to %v", got)
	}
}

// Pods that are not FoundationDB processes, and pods already gone, are
// evicted as usual.
func TestOtherEvictionsAreAllowed(t *testing.T) {
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-fdb-cluster-exporter", Namespace: evictionNamespace}}
	h, _ := evictionHandler(t, other, fdbCluster(nil))

	for _, pod := range []string{"simplyblock-fdb-cluster-exporter", "simplyblock-fdb-cluster-log-gone"} {
		if resp := evict(t, h, pod, false); !resp.Allowed {
			t.Errorf("eviction of %s refused: %+v", pod, resp.Result)
		}
	}
}

// A process group that is fully excluded no longer holds the next one back,
// although the operator still reports it until it includes its address again.
func TestAnExcludedProcessGroupNoLongerBlocksTheNext(t *testing.T) {
	h, c := evictionHandler(t, fdbPod("simplyblock-fdb-cluster-log-1", "log-1"),
		fdbCluster([]string{"storage-1"}, "storage-1+excluded", "log-1"))

	wantRetry(t, evict(t, h, "simplyblock-fdb-cluster-log-1", false))
	got := toRemoveOf(t, c)
	if len(got) != 2 || got[1] != "log-1" {
		t.Errorf("processGroupsToRemove = %v, want [storage-1 log-1]", got)
	}
}
