// Package deferrers provides defer-friendly helpers that run a cleanup action
// (an io.Closer's Close, or a func() error teardown/cancel) and log any error
// instead of silently dropping it.
//
// Each log records the caller that scheduled the deferred call — the function,
// file and line — so a failing cleanup (often the sign of a leak) is visible
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
// callbacks that return an error, e.g. `defer deferrers.Run(tx.Rollback)`.
func Run(fn func() error) {
	if fn == nil {
		return
	}
	if err := fn(); err != nil {
		logErr(err, "cleanup")
	}
}

// logErr emits err at Error level, annotated with the caller of the exported
// helper (the function that deferred the call).
func logErr(err error, op string) {
	l := Logger
	if l == nil {
		l = slog.Default()
	}
	attrs := []any{slog.String("op", op), slog.Any("error", err)}
	// Caller(0)=logErr, (1)=Close/Run, (2)=the function that deferred the call.
	if pc, file, line, ok := runtime.Caller(2); ok {
		caller := "?"
		if fn := runtime.FuncForPC(pc); fn != nil {
			caller = fn.Name()
		}
		attrs = append(attrs,
			slog.String("caller", caller),
			slog.String("loc", fmt.Sprintf("%s:%d", file, line)),
		)
	}
	l.Error("deferred cleanup failed", attrs...)
}
