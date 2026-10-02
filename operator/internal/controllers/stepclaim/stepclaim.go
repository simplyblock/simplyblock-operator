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
	"errors"
	"fmt"
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
//
// The claim has to be the first status write of the pass after the step was
// chosen. A write that rereads and retries on a conflict refreshes obj to the
// current version, so a claim taken after one is a claim on whatever step the
// operation has since moved to, taken by a pass that chose its step from a stale
// copy. A record that has to be written before the call travels in also.
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
			return err
		}
		// obj is now what the API server answered with. A schema that predates
		// the claim prunes it and accepts the rest of the patch, and a claim
		// that was not stored is no claim: firing on it would leave nothing to
		// stop the next pass firing again.
		if !stored(*step, claimed) {
			return fmt.Errorf("%w: %s %s", ErrClaimNotStored,
				reflect.TypeOf(obj).Elem().Name(), client.ObjectKeyFromObject(obj))
		}
		return nil
	}
}

// ErrClaimNotStored is a claim the API server accepted the patch for and did
// not keep, which is what a CRD installed before the claim field does. The
// step's call is not made until the CRDs are upgraded.
var ErrClaimNotStored = errors.New(
	"the API server did not store the step's claim; the installed CRD predates it, " +
		"and the call is held until the CRDs are upgraded")

// stored reports whether the snapshot the API server answered with carries the
// claim that was written. The lease is compared in whole seconds, because that
// is the precision a status timestamp is stored at.
func stored(answered, claimed statemachine.KubeSnapshot) bool {
	got, want := answered.Claim, claimed.Claim
	if got == nil || want == nil {
		return got == want
	}
	return got.State == want.State && got.Attempt == want.Attempt &&
		got.LeaseUntil.Unix() == want.LeaseUntil.Unix()
}
