// Package locks provides helpers that scope a mutex to a single function call,
// so critical sections read as one expression instead of a Lock/Unlock pair the
// reader has to match up by eye. Every helper unlocks with defer, which keeps
// the mutex from being left held when the transaction returns early or panics.
//
// The helpers follow a naming scheme:
//
//   - With… takes a transaction returning error, and passes that error through.
//   - Via… takes a transaction returning nothing.
//   - …Value returns a result value alongside the error.
//   - …TryLock… never blocks: it reports whether the lock was acquired, and
//     runs the transaction only if it was.
//
// For the Try variants the returned bool reports lock acquisition, not the
// outcome of the transaction. A false result therefore means the transaction
// never ran, and any accompanying error or value is the zero value.
//
// Because sync.Mutex is not reentrant, a transaction must not lock the same
// mutex again, directly or through another helper, or it deadlocks.
package locks

import "sync"

// WithLock runs transaction while holding lock and returns its error.
func WithLock(lock *sync.Mutex, transaction func() error) error {
	lock.Lock()
	defer lock.Unlock()
	return transaction()
}

// WithTryLock runs transaction while holding lock, but only if lock is
// available right away. It reports whether the lock was acquired. When it was
// not, transaction is skipped and the error is nil.
func WithTryLock(lock *sync.Mutex, transaction func() error) (bool, error) {
	if !lock.TryLock() {
		return false, nil
	}
	defer lock.Unlock()
	return true, transaction()
}

// WithLockValue runs transaction while holding lock and returns its result and
// error.
func WithLockValue[R any](lock *sync.Mutex, transaction func() (R, error)) (R, error) {
	lock.Lock()
	defer lock.Unlock()
	return transaction()
}

// WithTryLockValue runs transaction while holding lock, but only if lock is
// available right away. It reports whether the lock was acquired. When it was
// not, transaction is skipped and the zero value of R and a nil error are
// returned.
func WithTryLockValue[R any](lock *sync.Mutex, transaction func() (R, error)) (bool, R, error) {
	if !lock.TryLock() {
		var zero R
		return false, zero, nil
	}
	defer lock.Unlock()
	r, err := transaction()
	return true, r, err
}

// ViaLock runs transaction while holding lock.
func ViaLock(lock *sync.Mutex, transaction func()) {
	lock.Lock()
	defer lock.Unlock()
	transaction()
}

// ViaTryLock runs transaction while holding lock, but only if lock is available
// right away. It reports whether the lock was acquired, and hence whether
// transaction ran.
func ViaTryLock(lock *sync.Mutex, transaction func()) bool {
	if !lock.TryLock() {
		return false
	}
	defer lock.Unlock()
	transaction()
	return true
}
