package csicommon

import (
	"testing"
	"time"
)

func TestIDLocker(t *testing.T) {
	t.Parallel()
	fakeID := "fake-id"
	locks := NewVolumeLocks()
	// acquire lock for fake-id
	start := time.Now()
	unlock := locks.Lock(fakeID)
	go func() {
		time.Sleep(1 * time.Second)
		unlock()
	}()
	unlock2 := locks.Lock(fakeID)
	unlock2()
	duration := time.Since(start)
	// time.Sleep() might not be very accurate,
	// using 900 milliseconds to check the test would be more stable.
	if duration.Milliseconds() < 900 {
		t.Errorf("lock was required before it was released")
	}
}
