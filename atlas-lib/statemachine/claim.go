// Claiming the side effect of a stored state before firing it. A controller
// that persists its step reads the resource from an informer cache, and a cache
// lags the API server by the time a watch event takes to arrive. A pass that
// fired a state's side effect and then wrote the next state can be followed, in
// that window, by a pass that still reads the old state and fires the side
// effect again. A status write that lands after the call cannot prevent that:
// its conflict arrives once the call has already been made.
//
// The claim is the write that comes first. It records on the state itself that
// its side effect has started, conditional on the resource still being at the
// version the pass read, and only the pass whose write lands fires. A pass
// holding a stale copy writes against a version that no longer exists, gets a
// conflict, and fires nothing.
//
// The claim is a lease rather than a lock. Nothing releases it on success: the
// write that records the next state replaces the snapshot, and the claim goes
// with it. A process that crashes between the claim and that write leaves a
// claim that expires, after which the state's side effect may be fired again.
// Expiry does not prove the first call never landed, so a caller keeps the
// guard that reads whether the target is already where the call would put it,
// and runs it before every claim.
//
// It lives beside KubeSnapshot because the claim is part of the snapshot, and
// controller-gen copies a type's doc comment into every CRD that embeds it, so
// this header carries the reasoning.

package statemachine

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KubeClaim records that the side effect of a snapshot's state was started, and
// until when that start is trusted to still be in flight.
type KubeClaim struct {
	// Attempt counts the claims taken on this state, starting at 1.
	Attempt int32 `json:"attempt"`

	// LeaseUntil is when the claim expires and the side effect may be fired
	// again.
	LeaseUntil metav1.Time `json:"leaseUntil"`
}

// ClaimWriter persists claimed as the resource's snapshot, conditional on the
// resource still being at the version the caller read it at. When it is not,
// it returns the API server's Conflict error unchanged and does not retry.
type ClaimWriter func(ctx context.Context, claimed KubeSnapshot) error

// WithClaim claims the side effect of stored's state for lease, and runs fire
// only if the claim was acquired. It reports whether the claim was acquired.
// When it was not, fire is skipped and the error is nil.
func WithClaim(
	ctx context.Context, stored KubeSnapshot, lease time.Duration,
	write ClaimWriter, fire func() error,
) (bool, error) {
	now := time.Now()
	attempt := int32(1)
	if held := stored.Claim; held != nil {
		if now.Before(held.LeaseUntil.Time) {
			return false, nil
		}
		attempt = held.Attempt + 1
	}

	claimed := *stored.DeepCopy()
	claimed.Claim = &KubeClaim{Attempt: attempt, LeaseUntil: metav1.NewTime(now.Add(lease))}
	if err := write(ctx, claimed); err != nil {
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return true, fire()
}
