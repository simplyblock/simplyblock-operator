// Tests for the claim's write against an API server whose CRD predates the
// claim. The chart upgrades the operator before its CRDs are applied, and an old
// schema prunes status.step.claim while accepting the patch. A writer that took
// the accepted patch for a stored claim would fire the call with no lease
// behind it, and every later pass could fire it again.

package stepclaim_test

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/stepclaim"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// pruning is an API server whose schema has no status.step.claim: it applies a
// status patch and answers with the object the claim was pruned from.
func pruning() interceptor.Funcs {
	return interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, c client.Client, subResource string,
			obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			if ops, ok := obj.(*simplyblockv1alpha2.StorageClusterOps); ok {
				ops.Status.Step.Claim = nil
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	}
}

func TestAClaimTheAPIServerDidNotStoreIsNotTakenForOne(t *testing.T) {
	ctx := context.Background()
	deadline := metav1.NewTime(time.Now().Add(time.Minute))
	ops := &simplyblockv1alpha2.StorageClusterOps{
		ObjectMeta: metav1.ObjectMeta{Name: "activate", Namespace: "sb"},
		Status: simplyblockv1alpha2.StorageClusterOpsStatus{
			Step: statemachine.KubeSnapshot{State: "Requesting", Deadline: &deadline},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testsupport.NewScheme(t)).
		WithObjects(ops).
		WithStatusSubresource(&simplyblockv1alpha2.StorageClusterOps{}).
		WithInterceptorFuncs(pruning()).
		Build()
	if err := c.Get(ctx, client.ObjectKeyFromObject(ops), ops); err != nil {
		t.Fatalf("read the operation: %v", err)
	}

	fired := 0
	acquired, err := statemachine.WithClaim(ctx, ops.Status.Step, time.Minute,
		stepclaim.Writer(c, ops, &ops.Status.Step), func() error { fired++; return nil })

	if !errors.Is(err, stepclaim.ErrClaimNotStored) {
		t.Errorf("err = %v, want ErrClaimNotStored", err)
	}
	if acquired || fired != 0 {
		t.Errorf("acquired = %v, fired %d times, want no call without a stored claim",
			acquired, fired)
	}
}
