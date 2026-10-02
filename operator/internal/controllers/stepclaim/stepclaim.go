// The Kubernetes half of statemachine.WithClaim: the write that records a claim
// on an operation's step in its status subresource. atlas-lib takes the write as
// a callback so that it does not depend on controller-runtime, and every *Ops
// controller here would otherwise write the same callback against its own kind.
//
// It is a package of its own because the controllers are one package per domain
// (design-crd-model.md §7.10), and a helper declared in one cannot be used by
// another.

package stepclaim

import (
	"context"
	"reflect"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/statemachine"
)

// Writer returns the ClaimWriter for an operation. It patches obj's status with
// the claimed snapshot stored through step, which points into obj's status, and
// applies also in the same patch. The patch carries an optimistic lock on the
// resourceVersion obj was read at and is never retried, because a conflict is
// the answer the claim asks for.
//
// On success obj is what the API server returned, so a later write in the same
// pass starts from the version the claim produced. On failure obj is restored
// to what it was before the write.
func Writer(
	c client.Client, obj client.Object, step *statemachine.KubeSnapshot, also ...func(),
) statemachine.ClaimWriter {
	return func(ctx context.Context, claimed statemachine.KubeSnapshot) error {
		read, ok := obj.DeepCopyObject().(client.Object)
		if !ok {
			panic("stepclaim: the object's deep copy is not a client.Object")
		}
		*step = claimed
		for _, mutate := range also {
			mutate()
		}
		err := c.Status().Patch(ctx, obj,
			client.MergeFromWithOptions(read, client.MergeFromWithOptimisticLock{}))
		if err != nil {
			reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(read).Elem())
		}
		return err
	}
}
