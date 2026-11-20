// Package deferrers provides defer-friendly helpers that run a cleanup action
// (an io.Closer's Close, or a func() error teardown/cancel) and log any error
// instead of silently dropping it.
//
// Each log records the caller that scheduled the deferred call (the function,
// file, and line) so a failing cleanup, often the sign of a leak, is visible
// with its origin, rather than disappearing behind `defer f.Close()`.
//
// Typical use:
//
//	f, err := os.Open(name)
//	if err != nil {
//		return err
//	}
//	defer deferrers.Close(f)
//
//	tx, err := db.Begin()
//	if err != nil {
//		return err
//	}
//	defer deferrers.Run(tx.Rollback)
//
// CloseFn and RunFn return the same cleanup as a func() for APIs that take one
// rather than a call, and log the site they were registered from:
//
//	t.Cleanup(deferrers.CloseFn(conn))
package deferrers

import (
	"fmt"
	"io"
	"log/slog"
	"runtime"
)

// Logger is used to emit cleanup-failure logs. When nil, slog.Default() is
// resolved at log time so it follows the application's configured default.
var Logger *slog.Logger

// Close closes c and logs any error, annotated with the caller that scheduled
// the deferred call and the closer's concrete type. A nil c is a no-op.
func Close(c io.Closer) {
	if c == nil {
		return
	}
	if err := c.Close(); err != nil {
		logErr(err, fmt.Sprintf("close %T", c))
	}
}

// Run invokes fn and logs any error, annotated with the caller that scheduled
// the deferred call. A nil fn is a no-op. Use it for teardown / cancel style
// callbacks that return an error, e.g., `defer deferrers.Run(tx.Rollback)`.
func Run(fn func() error) {
	if fn == nil {
		return
	}
	if err := fn(); err != nil {
		logErr(err, "cleanup")
	}
}

// CloseFn returns a func that closes c and logs any error, for APIs that take
// a func() rather than running a call, such as t.Cleanup:
//
//	t.Cleanup(deferrers.CloseFn(conn))
//
// The log names the caller of CloseFn, the place the cleanup was registered,
// not whatever eventually runs it. A nil c returns a no-op.
func CloseFn(c io.Closer) func() {
	if c == nil {
		return func() {}
	}
	site := callerSite(2)
	return func() {
		if err := c.Close(); err != nil {
			logErrAt(err, fmt.Sprintf("close %T", c), site)
		}
	}
}

// RunFn is CloseFn for a func() error, such as
// t.Cleanup(deferrers.RunFn(func() error { return os.RemoveAll(dir) })).
func RunFn(fn func() error) func() {
	if fn == nil {
		return func() {}
	}
	site := callerSite(2)
	return func() {
		if err := fn(); err != nil {
			logErrAt(err, "cleanup", site)
		}
	}
}

// site is where a cleanup was scheduled: the function and its file and line.
type site struct {
	caller string
	loc    string
	ok     bool
}

// callerSite returns the frame skip levels up the stack, counting callerSite
// itself as 0.
func callerSite(skip int) site {
	pc, file, line, ok := runtime.Caller(skip)
	if !ok {
		return site{}
	}
	caller := "?"
	if fn := runtime.FuncForPC(pc); fn != nil {
		caller = fn.Name()
	}
	return site{caller: caller, loc: fmt.Sprintf("%s:%d", file, line), ok: true}
}

// logErr emits err at Error level, annotated with the caller of the exported
// helper (the function that deferred the call).
func logErr(err error, op string) {
	// (1)=logErr, (2)=Close/Run, (3)=the function that deferred the call.
	logErrAt(err, op, callerSite(3))
}

// logErrAt emits err at Error level, annotated with the site the cleanup was
// scheduled from.
func logErrAt(err error, op string, s site) {
	l := Logger
	if l == nil {
		l = slog.Default()
	}
	attrs := []any{slog.String("op", op), slog.Any("error", err)}
	if s.ok {
		attrs = append(attrs, slog.String("caller", s.caller), slog.String("loc", s.loc))
	}
	l.Error("deferred cleanup failed", attrs...)
}
