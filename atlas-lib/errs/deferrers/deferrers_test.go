package deferrers

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type errCloser struct{ err error }

func (e errCloser) Close() error { return e.err }

func captureLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	Logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { Logger = nil })
	return buf
}

func TestClose_LogsErrorWithCaller(t *testing.T) {
	buf := captureLogger(t)

	Close(errCloser{err: errors.New("boom")})

	out := buf.String()
	for _, want := range []string{"boom", "op=", "close", "caller=", "TestClose_LogsErrorWithCaller", "loc="} {
		if !strings.Contains(out, want) {
			t.Fatalf("log %q missing %q", out, want)
		}
	}
}

func TestClose_NoErrorAndNil(t *testing.T) {
	buf := captureLogger(t)

	Close(nil)
	Close(errCloser{err: nil})

	if buf.Len() != 0 {
		t.Fatalf("expected no logs, got %q", buf.String())
	}
}

func TestRun_LogsErrorWithCaller(t *testing.T) {
	buf := captureLogger(t)

	Run(func() error { return errors.New("cleanup-fail") })

	out := buf.String()
	for _, want := range []string{"cleanup-fail", "TestRun_LogsErrorWithCaller"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log %q missing %q", out, want)
		}
	}
}

func TestRun_NoErrorAndNil(t *testing.T) {
	buf := captureLogger(t)

	Run(nil)
	Run(func() error { return nil })

	if buf.Len() != 0 {
		t.Fatalf("expected no logs, got %q", buf.String())
	}
}

// countingCloser records how often it was closed.
type countingCloser struct {
	closed int
	err    error
}

func (c *countingCloser) Close() error {
	c.closed++
	return c.err
}

// runElsewhere stands for t.Cleanup or any other runner: the func runs from a
// different stack than the one that created it.
func runElsewhere(fn func()) { fn() }

func TestCloseFn_ClosesOnlyWhenRun(t *testing.T) {
	captureLogger(t)
	c := &countingCloser{}

	fn := CloseFn(c)
	if c.closed != 0 {
		t.Fatalf("CloseFn closed at registration (%d times)", c.closed)
	}
	runElsewhere(fn)
	if c.closed != 1 {
		t.Fatalf("closed %d times after running the func, want 1", c.closed)
	}
}

// The func runs from wherever it was handed to, typically the testing
// package's cleanup loop. Reporting that caller would point every failed
// cleanup at the same line in the standard library.
func TestCloseFn_LogsTheRegistrationSite(t *testing.T) {
	buf := captureLogger(t)

	fn := CloseFn(&countingCloser{err: errors.New("boom")}) // registration site
	runElsewhere(fn)

	out := buf.String()
	for _, want := range []string{"boom", "close", "caller=", "TestCloseFn_LogsTheRegistrationSite"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log %q missing %q", out, want)
		}
	}
	if strings.Contains(out, "runElsewhere") {
		t.Fatalf("log %q names the runner, not the registration site", out)
	}
}

func TestCloseFn_NoErrorAndNil(t *testing.T) {
	buf := captureLogger(t)

	runElsewhere(CloseFn(nil))
	runElsewhere(CloseFn(&countingCloser{}))

	if buf.Len() != 0 {
		t.Fatalf("expected no logs, got %q", buf.String())
	}
}

func TestRunFn_RunsOnlyWhenRunAndLogsTheRegistrationSite(t *testing.T) {
	buf := captureLogger(t)
	calls := 0

	fn := RunFn(func() error { calls++; return errors.New("cleanup-fail") })
	if calls != 0 {
		t.Fatalf("RunFn ran at registration")
	}
	runElsewhere(fn)

	out := buf.String()
	if calls != 1 || !strings.Contains(out, "cleanup-fail") ||
		!strings.Contains(out, "TestRunFn_RunsOnlyWhenRunAndLogsTheRegistrationSite") {
		t.Fatalf("calls = %d, log %q", calls, out)
	}
	if strings.Contains(out, "runElsewhere") {
		t.Fatalf("log %q names the runner, not the registration site", out)
	}
}

func TestRunFn_NoErrorAndNil(t *testing.T) {
	buf := captureLogger(t)

	runElsewhere(RunFn(nil))
	runElsewhere(RunFn(func() error { return nil }))

	if buf.Len() != 0 {
		t.Fatalf("expected no logs, got %q", buf.String())
	}
}
