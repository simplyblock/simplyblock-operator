package hugepages

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/simplyblock-operator/internal/discovery"
)

const testClusterUUID = "11111111-1111-1111-1111-111111111111"

// stubEstimator is the "stub controlplane.Client" the test plan calls for: a
// fixed EstimateHugePages result or a fixed error, no server involved.
type stubEstimator struct {
	mib int64
	err error
}

func (s stubEstimator) EstimateHugePages(context.Context, int32, int32) (int64, error) {
	return s.mib, s.err
}

func workerWithHugePages(name string, allocatableMiB int64) discovery.KubeNode {
	return discovery.KubeNode{
		Name: name,
		HugePages: []discovery.KubeHugePages{
			{SizeBytes: twoMiBBytes, CapacityBytes: allocatableMiB * mibBytes, AllocatableBytes: allocatableMiB * mibBytes},
		},
	}
}

func hugePagesPod(name, node string, miB int64, clusterLabelValue string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: "c",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						hugePages2MiResource: *resource.NewQuantity(miB*1024*1024, resource.BinarySI),
					},
				},
			}},
		},
	}
	if clusterLabelValue != "" {
		pod.Labels = map[string]string{clusterLabel: clusterLabelValue}
	}
	return pod
}

func newFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithObjects(objs...).Build()
}

// --- Step 1: required huge pages (U-01..U-05) ---

func TestEvaluateRequiredMiB(t *testing.T) {
	for _, tc := range []struct {
		name                string
		formulaMiB          int64
		minHugePagesSizeMiB int64
		wantRequiredMiB     int64
	}{
		// U-01: formula below the floor, floor wins. floor=200 -> (200/2)*2+512=712
		{"floor wins", 50, 200, 712},
		// U-02: formula above the floor, formula wins. 300 -> 300+512=812
		{"formula wins", 300, 50, 812},
		// U-03: no floor set, bare formula is used. 100 -> 100+512=612
		{"no floor", 100, 0, 612},
		// U-04: odd-MiB formula rounds down to the nearest even MiB, then +512.
		// 101 -> (101/2)*2+512 = 100+512 = 612
		{"odd rounds down", 101, 0, 612},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k8sClient := newFakeClient()
			cp := stubEstimator{mib: tc.formulaMiB}
			worker := workerWithHugePages("w1", tc.wantRequiredMiB+1000) // plenty, isolate Step 1

			result, err := Evaluate(context.Background(), k8sClient, cp,
				[]discovery.KubeNode{worker}, testClusterUUID, 40, 4, tc.minHugePagesSizeMiB)
			if err != nil {
				t.Fatal(err)
			}
			if result.RequiredMiB != tc.wantRequiredMiB {
				t.Errorf("RequiredMiB = %d, want %d", result.RequiredMiB, tc.wantRequiredMiB)
			}
		})
	}
}

// U-05: an estimate error is returned as-is, with no partial Result.
func TestEvaluateEstimateError(t *testing.T) {
	k8sClient := newFakeClient()
	cp := stubEstimator{err: errors.New("control plane unreachable")}

	result, err := Evaluate(context.Background(), k8sClient, cp,
		[]discovery.KubeNode{workerWithHugePages("w1", 10000)}, testClusterUUID, 40, 4, 0)
	if err == nil {
		t.Fatal("want an error")
	}
	if result != nil {
		t.Errorf("result = %+v, want nil", result)
	}
}

// --- Steps 2-3: capacity and contention (U-06..U-14) ---

func TestEvaluateCapacityAndContention(t *testing.T) {
	// requiredMiB with formula=100, no floor: (100/2)*2+512 = 612
	const formulaMiB = 100
	const requiredMiB = 612

	t.Run("U-06 no hugepages-2Mi entry at all", func(t *testing.T) {
		worker := discovery.KubeNode{Name: "w1"}
		result, err := Evaluate(context.Background(), newFakeClient(), stubEstimator{mib: formulaMiB},
			[]discovery.KubeNode{worker}, testClusterUUID, 40, 4, 0)
		if err != nil {
			t.Fatal(err)
		}
		assertFailed(t, result, "w1", ReasonInsufficientHugePages)
	})

	t.Run("U-07 allocatable below required", func(t *testing.T) {
		worker := workerWithHugePages("w1", requiredMiB-1)
		result, err := Evaluate(context.Background(), newFakeClient(), stubEstimator{mib: formulaMiB},
			[]discovery.KubeNode{worker}, testClusterUUID, 40, 4, 0)
		if err != nil {
			t.Fatal(err)
		}
		assertFailed(t, result, "w1", ReasonInsufficientHugePages)
	})

	t.Run("U-08 allocatable exactly equal, no other pods, passes", func(t *testing.T) {
		worker := workerWithHugePages("w1", requiredMiB)
		result, err := Evaluate(context.Background(), newFakeClient(), stubEstimator{mib: formulaMiB},
			[]discovery.KubeNode{worker}, testClusterUUID, 40, 4, 0)
		if err != nil {
			t.Fatal(err)
		}
		assertPassed(t, result, "w1")
	})

	t.Run("U-09 enough capacity, zero other pods, passes", func(t *testing.T) {
		worker := workerWithHugePages("w1", requiredMiB+1000)
		result, err := Evaluate(context.Background(), newFakeClient(), stubEstimator{mib: formulaMiB},
			[]discovery.KubeNode{worker}, testClusterUUID, 40, 4, 0)
		if err != nil {
			t.Fatal(err)
		}
		assertPassed(t, result, "w1")
	})

	t.Run("U-10 unrelated pod leaves exactly the required amount free: passes", func(t *testing.T) {
		worker := workerWithHugePages("w1", requiredMiB+500)
		pod := hugePagesPod("other-pod", "w1", 500, "")
		result, err := Evaluate(context.Background(), newFakeClient(pod), stubEstimator{mib: formulaMiB},
			[]discovery.KubeNode{worker}, testClusterUUID, 40, 4, 0)
		if err != nil {
			t.Fatal(err)
		}
		assertPassed(t, result, "w1")
	})

	t.Run("U-11 unrelated pod leaves one MiB less than required: contended, names the pod", func(t *testing.T) {
		worker := workerWithHugePages("w1", requiredMiB+500)
		pod := hugePagesPod("other-pod", "w1", 501, "")
		result, err := Evaluate(context.Background(), newFakeClient(pod), stubEstimator{mib: formulaMiB},
			[]discovery.KubeNode{worker}, testClusterUUID, 40, 4, 0)
		if err != nil {
			t.Fatal(err)
		}
		w := assertFailed(t, result, "w1", ReasonHugePagesContended)
		if !strings.Contains(w.Message, "other-pod") {
			t.Errorf("Message = %q, want it to name other-pod", w.Message)
		}
	})

	t.Run("U-12 a pod of this cluster's own storage nodes is excluded from contention", func(t *testing.T) {
		worker := workerWithHugePages("w1", requiredMiB+500)
		// Holds enough that, if counted, it would fail Step 3 -- but it is this
		// cluster's own, so Evaluate must not count it.
		pod := hugePagesPod("own-node", "w1", 501, testClusterUUID)
		result, err := Evaluate(context.Background(), newFakeClient(pod), stubEstimator{mib: formulaMiB},
			[]discovery.KubeNode{worker}, testClusterUUID, 40, 4, 0)
		if err != nil {
			t.Fatal(err)
		}
		assertPassed(t, result, "w1")
	})

	t.Run("U-13 multiple workers, one failing and one passing, reported independently", func(t *testing.T) {
		failing := workerWithHugePages("bad", requiredMiB-1)
		passing := workerWithHugePages("good", requiredMiB+1000)
		result, err := Evaluate(context.Background(), newFakeClient(), stubEstimator{mib: formulaMiB},
			[]discovery.KubeNode{failing, passing}, testClusterUUID, 40, 4, 0)
		if err != nil {
			t.Fatal(err)
		}
		assertFailed(t, result, "bad", ReasonInsufficientHugePages)
		assertPassed(t, result, "good")
	})

	t.Run("U-14 zero candidate workers: empty, non-failing Result", func(t *testing.T) {
		result, err := Evaluate(context.Background(), newFakeClient(), stubEstimator{mib: formulaMiB},
			nil, testClusterUUID, 40, 4, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Workers) != 0 {
			t.Errorf("Workers = %+v, want none", result.Workers)
		}
		if !result.AllReady() {
			t.Errorf("AllReady() = false, want true for an empty Result")
		}
	})
}

func assertFailed(t *testing.T, result *Result, worker, reason string) WorkerResult {
	t.Helper()
	for _, w := range result.Workers {
		if w.Name == worker {
			if w.Ready {
				t.Fatalf("worker %s: Ready = true, want false (%s)", worker, reason)
			}
			if w.Reason != reason {
				t.Fatalf("worker %s: Reason = %q, want %q", worker, w.Reason, reason)
			}
			return w
		}
	}
	t.Fatalf("no result for worker %s in %+v", worker, result.Workers)
	return WorkerResult{}
}

func assertPassed(t *testing.T, result *Result, worker string) WorkerResult {
	t.Helper()
	for _, w := range result.Workers {
		if w.Name == worker {
			if !w.Ready {
				t.Fatalf("worker %s: Ready = false (%s: %s), want true", worker, w.Reason, w.Message)
			}
			return w
		}
	}
	t.Fatalf("no result for worker %s in %+v", worker, result.Workers)
	return WorkerResult{}
}

func TestMinHugePagesSizeMiB(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int64
	}{
		{"", 0},
		// ParseSize reads a bare unit as SI (decimal), matching
		// corev1.Quantity's own G/T: 100G is 100e9 bytes, not 100 GiB.
		{"100G", 100_000_000_000 / mibBytes},
		{"1T", 1_000_000_000_000 / mibBytes},
	} {
		if got := MinHugePagesSizeMiB(tc.raw); got != tc.want {
			t.Errorf("MinHugePagesSizeMiB(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}
