// Tests for WithClaim: the write that has to land before a state's side effect
// fires, the conflict that stops a pass holding a stale copy, and the lease that
// lets a crashed claim be taken again.

package statemachine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/simplyblock/atlas/statemachine"
)

// claimRig records what WithClaim wrote and how often it fired.
type claimRig struct {
	writeErr error
	written  []statemachine.KubeSnapshot
	fired    int
	fireErr  error
}

func (r *claimRig) write(_ context.Context, claimed statemachine.KubeSnapshot) error {
	r.written = append(r.written, claimed)
	return r.writeErr
}

func (r *claimRig) fire() error {
	r.fired++
	return r.fireErr
}

func requesting() statemachine.KubeSnapshot {
	deadline := metav1.NewTime(time.Now().Add(2 * time.Minute))
	return statemachine.KubeSnapshot{State: "Requesting", Deadline: &deadline}
}

func conflict() error {
	return apierrors.NewConflict(schema.GroupResource{Resource: "storageclusterops"},
		"activate", errors.New("the object has been modified"))
}

func TestWithClaim_UnclaimedStateIsClaimedThenFired(t *testing.T) {
	rig := &claimRig{}
	stored := requesting()
	before := time.Now()

	acquired, err := statemachine.WithClaim(context.Background(), stored, 30*time.Second,
		rig.write, rig.fire)
	if err != nil {
		t.Fatalf("WithClaim: %v", err)
	}
	if !acquired {
		t.Fatal("acquired = false, want the claim on an unclaimed state")
	}
	if rig.fired != 1 {
		t.Fatalf("fired %d times, want 1", rig.fired)
	}
	if len(rig.written) != 1 {
		t.Fatalf("wrote %d claims, want 1 before the side effect", len(rig.written))
	}
	claimed := rig.written[0]
	if claimed.State != stored.State || !claimed.Deadline.Equal(stored.Deadline) {
		t.Errorf("the claim changed the state or its deadline: %+v", claimed)
	}
	if claimed.Claim == nil {
		t.Fatal("the written snapshot carries no claim")
	}
	if claimed.Claim.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", claimed.Claim.Attempt)
	}
	if lease := claimed.Claim.LeaseUntil.Sub(before); lease < 30*time.Second || lease > 31*time.Second {
		t.Errorf("the lease runs %v from the call, want 30s", lease)
	}
}

// The incident: a pass reading a stale copy writes against a version that is
// gone, and must not fire.
func TestWithClaim_ConflictFiresNothing(t *testing.T) {
	rig := &claimRig{writeErr: conflict()}

	acquired, err := statemachine.WithClaim(context.Background(), requesting(), 30*time.Second,
		rig.write, rig.fire)
	if err != nil {
		t.Fatalf("WithClaim: %v, want a conflict reported as a claim not acquired", err)
	}
	if acquired {
		t.Error("acquired = true on a conflict")
	}
	if rig.fired != 0 {
		t.Errorf("fired %d times after a conflict, want 0", rig.fired)
	}
}

func TestWithClaim_FailedWriteFiresNothingAndReturnsTheError(t *testing.T) {
	unreachable := errors.New("connection refused")
	rig := &claimRig{writeErr: unreachable}

	acquired, err := statemachine.WithClaim(context.Background(), requesting(), 30*time.Second,
		rig.write, rig.fire)
	if !errors.Is(err, unreachable) {
		t.Fatalf("err = %v, want the write's error", err)
	}
	if acquired || rig.fired != 0 {
		t.Errorf("acquired = %v, fired %d times, want neither on a failed write", acquired, rig.fired)
	}
}

func TestWithClaim_LiveClaimIsNeitherRetakenNorFired(t *testing.T) {
	rig := &claimRig{}
	stored := requesting()
	stored.Claim = &statemachine.KubeClaim{
		State:      stored.State,
		Attempt:    1,
		LeaseUntil: metav1.NewTime(time.Now().Add(20 * time.Second)),
	}

	acquired, err := statemachine.WithClaim(context.Background(), stored, 30*time.Second,
		rig.write, rig.fire)
	if err != nil {
		t.Fatalf("WithClaim: %v", err)
	}
	if acquired || rig.fired != 0 || len(rig.written) != 0 {
		t.Errorf("acquired = %v, fired %d, wrote %d, want nothing under a live claim",
			acquired, rig.fired, len(rig.written))
	}
}

// A claim whose process crashed before the next state was written expires, and
// the next pass takes it again.
func TestWithClaim_ExpiredClaimIsRetakenAsTheNextAttempt(t *testing.T) {
	rig := &claimRig{}
	stored := requesting()
	stored.Claim = &statemachine.KubeClaim{
		State:      stored.State,
		Attempt:    2,
		LeaseUntil: metav1.NewTime(time.Now().Add(-time.Second)),
	}

	acquired, err := statemachine.WithClaim(context.Background(), stored, 30*time.Second,
		rig.write, rig.fire)
	if err != nil {
		t.Fatalf("WithClaim: %v", err)
	}
	if !acquired || rig.fired != 1 {
		t.Fatalf("acquired = %v, fired %d, want the expired claim retaken and fired once",
			acquired, rig.fired)
	}
	if got := rig.written[0].Claim.Attempt; got != 3 {
		t.Errorf("Attempt = %d, want 3", got)
	}
}

// A side effect that failed after the claim landed may still have reached its
// target, so its error is returned with the claim held.
func TestWithClaim_SideEffectErrorIsReturnedWithTheClaimHeld(t *testing.T) {
	refused := errors.New("503 from the control plane")
	rig := &claimRig{fireErr: refused}

	acquired, err := statemachine.WithClaim(context.Background(), requesting(), 30*time.Second,
		rig.write, rig.fire)
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the side effect's error", err)
	}
	if !acquired {
		t.Error("acquired = false, want true: the claim landed before the call failed")
	}
	if len(rig.written) != 1 {
		t.Errorf("wrote %d times, want only the claim and no release", len(rig.written))
	}
}

func TestKubeSnapshot_DeepCopyDoesNotShareTheClaim(t *testing.T) {
	original := requesting()
	original.Claim = &statemachine.KubeClaim{Attempt: 1, LeaseUntil: metav1.Now()}

	copied := original.DeepCopy()
	copied.Claim.Attempt = 7

	if original.Claim.Attempt != 1 {
		t.Fatalf("Attempt = %d after changing the copy, want the original untouched",
			original.Claim.Attempt)
	}
}

// A controller that moves to the next state by editing the stored snapshot in
// place, rather than by replacing it, carries the previous state's claim along.
// That claim is not a claim on the new state, whose side effect has not been
// started.
func TestWithClaim_AClaimTakenInAnotherStateDoesNotHoldThisOne(t *testing.T) {
	rig := &claimRig{}
	stored := requesting()
	stored.State = "Failing"
	stored.Claim = &statemachine.KubeClaim{
		State:      "Removing",
		Attempt:    1,
		LeaseUntil: metav1.NewTime(time.Now().Add(20 * time.Second)),
	}

	acquired, err := statemachine.WithClaim(context.Background(), stored, 30*time.Second,
		rig.write, rig.fire)
	if err != nil {
		t.Fatalf("WithClaim: %v", err)
	}
	if !acquired || rig.fired != 1 {
		t.Fatalf("acquired = %v, fired %d, want the new state claimed and fired once",
			acquired, rig.fired)
	}
	claim := rig.written[0].Claim
	if claim.State != "Failing" || claim.Attempt != 1 {
		t.Errorf("claim = %+v, want the first attempt on Failing", claim)
	}
}
