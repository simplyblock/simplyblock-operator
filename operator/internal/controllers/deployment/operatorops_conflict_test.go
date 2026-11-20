// What a losing status write costs a discovery run.
//
// A step does its work and then records that it did it. If the recording fails
// the step has not happened as far as the next pass is concerned, so the next
// pass does the work again — and the work of Writing is creating a document and
// telling a reviewer about it.
//
// The create survives that, because it is idempotent by name and says so. The
// events do not: a run that wrote one document reported writing it twice and
// reported finding it already there, which describes a race that did not happen
// to a reviewer reading the only record the run leaves.

package deployment

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// conflictOnce fails the first status write the way the API server does when
// something else has written the object since it was read.
func conflictOnce(remaining *int) interceptor.Funcs {
	// Both verbs are hooked, because which one a status write uses is the
	// reconciler's business and the conflict is the test's.
	conflict := func(name string) error {
		*remaining--
		return apierrors.NewConflict(
			schema.GroupResource{
				Group:    simplyblockv1alpha2.GroupVersion.Group,
				Resource: "operatorops",
			},
			name,
			errStale,
		)
	}
	return interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context, c client.Client, subResource string,
			obj client.Object, opts ...client.SubResourceUpdateOption,
		) error {
			if *remaining > 0 {
				return conflict(obj.GetName())
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(
			ctx context.Context, c client.Client, subResource string,
			obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			if *remaining > 0 {
				return conflict(obj.GetName())
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	}
}

// countingRecorder remembers how many events carried each reason, which is what
// separates a step that ran once from one that ran twice.
type countingRecorder struct {
	reasons map[string]int
}

func (r *countingRecorder) Eventf(
	_ runtime.Object, _ runtime.Object, _, reason, _, _ string, _ ...any,
) {
	if r.reasons == nil {
		r.reasons = map[string]int{}
	}
	r.reasons[reason]++
}

func (r *countingRecorder) count(reason string) int { return r.reasons[reason] }

var _ events.EventRecorder = (*countingRecorder)(nil)

// errStale is what the API server's conflict carries.
var errStale = errConflict{}

type errConflict struct{}

func (errConflict) Error() string {
	return "the object has been modified; please apply your changes to the latest version and try again"
}

// A status write that loses a race is retried rather than failing the step.
//
// Without it the step's own work is repeated on the next pass, and the work is
// not all repeatable: the document is, because creating it again finds it and
// says so, but the events describing the run are emitted a second time.
func TestAStatusWriteThatLosesARaceIsRetried(t *testing.T) {
	remaining := 1
	r := newRunnerWithInterceptors(t, conflictOnce(&remaining),
		discoverRun(nil), worker("worker-1"), worker("worker-2"))

	r.step() // start
	r.step() // inspect
	r.step() // probing: creates the Jobs

	for _, node := range []string{"worker-1", "worker-2"} {
		cm := reportConfigMap(t, node, "0000:5e:00.0", "0000:5f:00.0")
		if err := r.client.Create(context.Background(), cm); err != nil {
			t.Fatalf("write a report: %v", err)
		}
	}

	r.step() // probing: sees the reports, moves to Writing
	_, ops := r.step()

	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseSucceeded {
		t.Fatalf("the run is %q after a conflict it should have retried: %s",
			ops.Status.Phase, ops.Status.Message)
	}
	if remaining != 0 {
		t.Error("the conflict was never raised, so this proves nothing")
	}
	if len(r.configs()) != 1 {
		t.Errorf("the run left %d documents", len(r.configs()))
	}
}

// The step's side effects happen once, which is what the retry is for: a second
// pass over Writing re-creates the document and finds it already there, and
// ConfigExists is a race being reported to somebody who did not have one.
func TestALostStatusWriteDoesNotReportAPhantomRace(t *testing.T) {
	remaining := 1
	rec := &countingRecorder{}
	r := newRunnerWithInterceptors(t, conflictOnce(&remaining),
		discoverRun(nil), worker("worker-1"), worker("worker-2"))
	r.reconciler.Recorder = rec

	r.step() // start
	r.step() // inspect
	r.step() // probing

	for _, node := range []string{"worker-1", "worker-2"} {
		cm := reportConfigMap(t, node, "0000:5e:00.0", "0000:5f:00.0")
		if err := r.client.Create(context.Background(), cm); err != nil {
			t.Fatalf("write a report: %v", err)
		}
	}

	r.step() // probing
	r.step() // writing

	if got := rec.count("ConfigExists"); got != 0 {
		t.Errorf("the run reported finding its own document %d time(s)", got)
	}
	if got := rec.count("OperationSucceeded"); got != 1 {
		t.Errorf("the run reported succeeding %d times", got)
	}
}
