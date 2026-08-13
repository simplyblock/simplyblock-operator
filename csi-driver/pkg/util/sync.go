package util

import "sync"

// withLock acquires mu, runs fn under it, and releases mu via defer.
// Using defer inside the helper gives every callsite a clean, scoped
// critical section without requiring a manual Unlock at each call site.
func withLock(mu *sync.Mutex, fn func()) {
	mu.Lock()
	defer mu.Unlock()
	fn()
}
