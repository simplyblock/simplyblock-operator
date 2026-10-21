// Tests for KlogLogger: a line names the code that logged it, not the
// adapter, so the path keeper's lines do not read as the watchdog's.

package agent

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"k8s.io/klog"
)

func TestKlogLoggerNamesTheCaller(t *testing.T) {
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	if err := fs.Set("logtostderr", "false"); err != nil {
		t.Fatalf("logtostderr: %v", err)
	}
	t.Cleanup(func() { _ = fs.Set("logtostderr", "true") })
	// The INFO output receives every severity, so one writer sees each line once.
	var out bytes.Buffer
	klog.SetOutputBySeverity("INFO", &out)

	KlogLogger{}.Infof("info from the test")
	KlogLogger{}.Warningf("warning from the test")
	klog.Flush()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var seen int
	for _, line := range lines {
		if !strings.Contains(line, "from the test") {
			continue
		}
		seen++
		if !strings.Contains(line, "logger_test.go:") {
			t.Errorf("logged %q, want it to name logger_test.go", line)
		}
	}
	if seen != 2 {
		t.Fatalf("found %d of the two lines in %q", seen, out.String())
	}
}
