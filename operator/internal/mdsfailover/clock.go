// The clock the decisions read, and the retry every API read goes through
// (design-pnfs-mds-standby.md §7.2).
//
// A clock is injected so the detection window and the 2-second retry spacing
// are tested with an instant clock instead of real waiting.

package mdsfailover

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Clock is time as the decisions see it.
type Clock interface {
	Now() time.Time
	// Sleep waits for d or until ctx ends.
	Sleep(ctx context.Context, d time.Duration) error
}

// RealClock is the wall clock.
type RealClock struct{}

// RealClock.Now reads the wall time.
func (RealClock) Now() time.Time { return time.Now() }

// Sleep waits for d or until ctx ends.
func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

const (
	// ReadRetries is how many times a failed API read is tried again.
	ReadRetries = 3
	// ReadRetryDelay is the pause between a failed read and its retry.
	ReadRetryDelay = 2 * time.Second
)

// readWithRetry runs fn, and again up to ReadRetries more times, ReadRetryDelay
// apart, while it fails. A NotFound is an answer and is not retried. The last
// error is returned when every try failed.
func readWithRetry[T any](ctx context.Context, clock Clock, fn func() (T, error)) (T, error) {
	var (
		out T
		err error
	)
	for attempt := 0; attempt <= ReadRetries; attempt++ {
		if attempt > 0 {
			if serr := clock.Sleep(ctx, ReadRetryDelay); serr != nil {
				return out, serr
			}
		}
		out, err = fn()
		if err == nil || apierrors.IsNotFound(err) {
			return out, err
		}
	}
	return out, err
}
