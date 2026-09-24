package nfsexport

import (
	"context"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/export"
)

// fakeExportAssembler is the inner delegate nfsdAssembler wraps.
type fakeExportAssembler struct {
	checked  int
	checkErr error
}

func (f *fakeExportAssembler) Create(context.Context, export.Spec) error { return nil }
func (f *fakeExportAssembler) Delete(context.Context, export.Spec) error { return nil }
func (f *fakeExportAssembler) Check(context.Context, export.Spec) error {
	f.checked++
	return f.checkErr
}

// validateThreadCount is checkNFSDThreads' decision, pulled out so it is
// testable without a real /proc/fs/nfsd, which a sandbox does not have.
func TestValidateThreadCount(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		wantErr bool
	}{
		{"running", "8\n", false},
		{"zero threads", "0", true},
		{"negative reads as unhealthy, not a crash", "-1", true},
		{"not a number", "not-a-number", true},
		{"empty", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateThreadCount([]byte(c.data))
			if (err != nil) != c.wantErr {
				t.Errorf("validateThreadCount(%q) error = %v, wantErr %v", c.data, err, c.wantErr)
			}
		})
	}
}

// nfsd is checked before the export sitting on it: an export cannot be well
// served by a kernel NFS server with no threads running, whatever its own
// mount and export-table state says. A sandbox has no /proc/fs/nfsd, so this
// also pins that the failure is reported rather than panicking past it.
func TestCheckFailsClosedWithoutReachingInnerWhenNFSDsControlFileIsAbsent(t *testing.T) {
	inner := &fakeExportAssembler{}
	assembler := nfsdAssembler{inner: inner}

	err := assembler.Check(context.Background(), export.Spec{Path: "/var/lib/simplyblock/exports/x"})
	if err == nil {
		t.Fatal("Check passed with no nfsd control filesystem present")
	}
	if !strings.Contains(err.Error(), "nfsd") {
		t.Errorf("error = %q, want it to name nfsd as the cause", err.Error())
	}
	if inner.checked != 0 {
		t.Errorf("inner.Check was reached %d times; nfsd's own health should have refused first", inner.checked)
	}
}
