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
