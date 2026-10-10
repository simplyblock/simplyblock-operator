// The `mds-runner extents` report: what the guest agent says a byte range of a
// file is made of, printed as JSON for a harness that runs it through kubectl
// exec.

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/simplyblock/atlas/fiemap"
	"github.com/simplyblock/atlas/nfsexport"
)

type fakeExtentAsker struct {
	got nfsexport.FileExtents
	err error
}

func (f fakeExtentAsker) FileExtents(
	_ context.Context, _, _ string, _, _ uint64,
) (nfsexport.FileExtents, error) {
	return f.got, f.err
}

func TestTheExtentReportIsTheAgentsAnswerAsJSON(t *testing.T) {
	var out bytes.Buffer
	asker := fakeExtentAsker{got: nfsexport.FileExtents{Size: 8192, Pieces: []fiemap.Piece{
		{Offset: 0, Length: 4096, Kind: fiemap.Written, Physical: 1 << 20},
		{Offset: 4096, Length: 4096, Kind: fiemap.Hole},
	}}}

	err := WriteExtentReport(context.Background(), asker, &out, "/var/lib/simplyblock/exports/a", "f.r1", 0, 8192)
	if err != nil {
		t.Fatalf("WriteExtentReport: %v", err)
	}
	var report ExtentReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out.String())
	}
	if report.Export != "/var/lib/simplyblock/exports/a" || report.File != "f.r1" || report.Size != 8192 ||
		len(report.Pieces) != 2 || report.Pieces[1].Kind != fiemap.Hole {
		t.Errorf("report = %+v", report)
	}
}

func TestAnAgentErrorIsReturnedAndNothingIsPrinted(t *testing.T) {
	var out bytes.Buffer
	err := WriteExtentReport(context.Background(), fakeExtentAsker{err: errors.New("refused")}, &out, "/x", "f", 0, 1)
	if err == nil || out.Len() != 0 {
		t.Errorf("err = %v, output %q; want the error and no output", err, out.String())
	}
}
