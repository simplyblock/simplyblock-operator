// Package bounded runs a blocking operation that no context can interrupt
// (a sysfs read, an ioctl, a device open, the wait for a child process) under a
// hard deadline, and gives up on it when the deadline passes.
//
// It exists because those operations block inside the kernel. A read of an NVMe
// controller's attribute or an Identify on a controller in error recovery can sit
// there until the controller is torn down, and a Go context does not reach into
// a system call. The caller of such an operation is often a loop that serves
// every volume on a node, so one stuck call used to stop all of them.
//
// Giving up does not end the operation: the goroutine running it stays blocked
// until the kernel lets it go, and holds its OS thread while it waits. Two rules
// keep that from accumulating:
//
//   - The deadline is short. These operations normally complete in microseconds,
//     so one that runs for a second is stuck rather than slow.
//   - A key that is still stuck fails at once. A caller passes a key naming what
//     it touches, usually the path, and while an earlier call on that key has
//     been given up on and not yet returned, a new call does not start a second
//     blocked goroutine behind it. Calls on the same key that are merely
//     concurrent are not affected: only abandoned calls count.
//
// A timeout is reported as an error wrapping context.DeadlineExceeded, which the
// errs/class classifier treats as a retryable timeout.
package bounded

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ReadTimeout is the budget for one read of kernel state: a sysfs attribute, a
// directory listing, a stat, a device open, or an Identify. All of these return
// in microseconds on a healthy host.
const ReadTimeout = time.Second

// Error reports an operation that was given up on.
type Error struct {
	// Key is what the operation touched, as its caller named it.
	Key string
	// After is how long the operation was given. It is zero when the operation
	// never started because an earlier one on the same key was still stuck.
	After time.Duration
}

func (e *Error) Error() string {
	if e.After == 0 {
		return fmt.Sprintf("%s: an earlier call is still stuck", e.Key)
	}
	return fmt.Sprintf("%s: no answer after %s", e.Key, e.After)
}

// Unwrap makes a timeout match context.DeadlineExceeded.
func (e *Error) Unwrap() error { return context.DeadlineExceeded }

var (
	stuckMu sync.Mutex
	stuck   = make(map[string]int) // key -> abandoned calls not yet returned
)

// Stuck reports whether an abandoned call on key has not yet returned.
func Stuck(key string) bool {
	stuckMu.Lock()
	defer stuckMu.Unlock()
	return stuck[key] > 0
}

// Do runs fn and returns its error, or an *Error once timeout passes with fn
// still running. See the package documentation for what happens to fn then.
func Do(key string, timeout time.Duration, fn func() error) error {
	_, err := Call(key, timeout, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

// Call runs fn and returns its results, or the zero value and an *Error once
// timeout passes with fn still running.
func Call[T any](key string, timeout time.Duration, fn func() (T, error)) (T, error) {
	var zero T
	if Stuck(key) {
		return zero, &Error{Key: key}
	}

	type result struct {
		v   T
		err error
	}
	var (
		mu        sync.Mutex
		finished  bool
		abandoned bool
	)
	done := make(chan result, 1)
	go func() {
		v, err := fn()
		mu.Lock()
		finished = true
		if abandoned {
			release(key)
		}
		mu.Unlock()
		done <- result{v, err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.v, r.err
	case <-timer.C:
	}

	mu.Lock()
	if finished {
		// It finished as the timer fired, and its result is already on the way.
		mu.Unlock()
		r := <-done
		return r.v, r.err
	}
	abandoned = true
	stuckMu.Lock()
	stuck[key]++
	stuckMu.Unlock()
	mu.Unlock()
	return zero, &Error{Key: key, After: timeout}
}

// release records that an abandoned call on key has finally returned.
func release(key string) {
	stuckMu.Lock()
	defer stuckMu.Unlock()
	if stuck[key]--; stuck[key] <= 0 {
		delete(stuck, key)
	}
}
