// The deadline and the stuck-call guard, driven by operations that block until
// a test releases them.

package bounded

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCallReturnsTheResultOfAnOperationThatFinishes(t *testing.T) {
	v, err := Call("finishes", time.Second, func() (int, error) { return 42, nil })
	if err != nil || v != 42 {
		t.Fatalf("Call = %d, %v, want 42, nil", v, err)
	}

	want := errors.New("the operation's own error")
	if err := Do("fails", time.Second, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("Do = %v, want the operation's own error", err)
	}
}

func TestCallGivesUpAtTheDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	start := time.Now()
	err := Do("gives-up", 50*time.Millisecond, func() error { <-release; return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do on a blocked operation = %v, want an error wrapping context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Do took %s to give up on a 50ms deadline", took)
	}
}

// A second call on a key whose first call is still stuck must not start another
// blocked goroutine: each one pins an OS thread for as long as the kernel holds
// it, and a loop retrying every few seconds would pile them up without bound.
func TestCallFailsAtOnceWhileTheSameKeyIsStuck(t *testing.T) {
	release := make(chan struct{})
	_ = Do("stuck-key", 20*time.Millisecond, func() error { <-release; return nil })

	ran := false
	start := time.Now()
	err := Do("stuck-key", time.Second, func() error { ran = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do on a stuck key = %v, want an error wrapping context.DeadlineExceeded", err)
	}
	if ran {
		t.Fatal("Do started the operation although the same key was still stuck")
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("Do on a stuck key took %s, want it to fail at once", took)
	}

	if err := Do("another-key", time.Second, func() error { return nil }); err != nil {
		t.Fatalf("a stuck key blocked a different key: %v", err)
	}

	// Once the stuck operation returns, the key is usable again.
	close(release)
	deadline := time.Now().Add(time.Second)
	for Stuck("stuck-key") {
		if time.Now().After(deadline) {
			t.Fatal("the key stayed stuck after its operation returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := Do("stuck-key", time.Second, func() error { return nil }); err != nil {
		t.Fatalf("Do after the stuck operation returned = %v, want nil", err)
	}
}

// Concurrent calls on one key are not stuck calls. Two loops reading the same
// attribute at once both have to get an answer.
func TestConcurrentCallsOnAKeyAreNotStuck(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			errs <- Do("concurrent", time.Second, func() error {
				time.Sleep(20 * time.Millisecond)
				return nil
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent call on a healthy key failed: %v", err)
		}
	}
}

// A process that has exited but left a child holding its output used to keep
// the caller waiting for as long as the child lived.
func TestCombinedOutputReturnsWhenAChildHoldsTheOutput(t *testing.T) {
	start := time.Now()
	out, err := CombinedOutput(context.Background(), 5*time.Second, "holds-output",
		"/bin/sh", "-c", "sleep 30 & echo started")
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("CombinedOutput took %s with a child holding the output", took)
	}
	if string(out) != "started\n" {
		t.Fatalf("CombinedOutput = %q, %v, want the output written before the parent exited", out, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CombinedOutput with a child holding the output = %v, want an error wrapping context.DeadlineExceeded", err)
	}
}

// Regression: 2026-10-04-unbounded-nvme-io — the Copilot review of #628 found
// both command timeouts returned errors that did not match
// context.DeadlineExceeded.
func TestCombinedOutputKillsAProcessAtTheDeadline(t *testing.T) {
	start := time.Now()
	_, err := CombinedOutput(context.Background(), 200*time.Millisecond, "never-exits",
		"/bin/sh", "-c", "exec sleep 30")
	// The kill surfaces from exec as a killed-by-signal error, which errs/class
	// would call an internal fault. A command that ran out of time has to read as a timeout.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CombinedOutput on a process killed at its deadline = %v, want an error wrapping context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("CombinedOutput took %s on a 200ms deadline", took)
	}
}
