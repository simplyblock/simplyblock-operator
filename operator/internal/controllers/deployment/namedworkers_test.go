// Naming the workers a discovery run inspects.
//
// A label selector cannot express "these two machines": its entries are ANDed,
// so two hostnames in one selector match nothing. spec.discover.workers is the
// list, and what these cover is that a name matching nothing is announced rather
// than quietly leaving the draft short.

package deployment

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aWorker is a node by name.
func aWorker(name string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// aDiscoveryRun is the reconciler with a recorder its events can be read from.
func aDiscoveryRun(t *testing.T) (*OperatorOpsReconciler, *simplyblockv1alpha2.OperatorOps, *events.FakeRecorder) {
	t.Helper()
	recorder := events.NewFakeRecorder(16)
	ops := &simplyblockv1alpha2.OperatorOps{
		ObjectMeta: metav1.ObjectMeta{Name: "rediscover", Namespace: "simplyblock"},
	}
	return &OperatorOpsReconciler{Recorder: recorder}, ops, recorder
}

// The named workers are the ones kept, and nothing else is.
func TestOnlyTheNamedWorkersAreInspected(t *testing.T) {
	r, ops, _ := aDiscoveryRun(t)

	kept := r.namedWorkers(ops,
		[]string{"worker-4.ocp.simplyblock.ai", "worker-5.ocp.simplyblock.ai"},
		[]corev1.Node{
			aWorker("worker-3.ocp.simplyblock.ai"),
			aWorker("worker-4.ocp.simplyblock.ai"),
			aWorker("worker-5.ocp.simplyblock.ai"),
		})

	if len(kept) != 2 {
		t.Fatalf("the run inspects %d workers, want the two it named", len(kept))
	}
	for _, node := range kept {
		if node.Name == "worker-3.ocp.simplyblock.ai" {
			t.Error("a worker the run did not name is inspected")
		}
	}
}

// TestANamedWorkerThatDoesNotExistIsAnnounced is why this is a function rather
// than a filter written inline.
//
// Being asked to inspect a machine that is not there is a typo or a node that
// has not joined. Both are worth saying: the alternative is a draft short of the
// machines somebody listed, with nothing anywhere saying which.
func TestANamedWorkerThatDoesNotExistIsAnnounced(t *testing.T) {
	r, ops, recorder := aDiscoveryRun(t)

	kept := r.namedWorkers(ops,
		[]string{"worker-4.ocp.simplyblock.ai", "worker-9.typo"},
		[]corev1.Node{aWorker("worker-4.ocp.simplyblock.ai")})

	if len(kept) != 1 {
		t.Fatalf("the run inspects %d workers", len(kept))
	}

	var announced string
	select {
	case announced = <-recorder.Events:
	default:
		t.Fatal("a named worker that does not exist was dropped without an event")
	}
	if !strings.Contains(announced, "worker-9.typo") {
		t.Errorf("the event does not name the worker: %s", announced)
	}
}

// Naming nothing is not the same as naming everything, and the caller is what
// decides not to filter. This asserts the helper is never asked to.
func TestNamingNoWorkerKeepsNothing(t *testing.T) {
	r, ops, _ := aDiscoveryRun(t)

	if kept := r.namedWorkers(ops, nil, []corev1.Node{aWorker("worker-4")}); len(kept) != 0 {
		t.Errorf("an empty name list kept %d workers, and the caller skips the filter "+
			"entirely for that case", len(kept))
	}
}
