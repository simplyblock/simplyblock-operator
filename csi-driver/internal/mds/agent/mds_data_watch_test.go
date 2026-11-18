// Tests for the watch over data pNFS clients send through the guest's nfsd.
// They run against a /proc tree written to a temporary directory, like the
// nfsd watchdog's.

package agent

import (
	"fmt"
	"strings"
	"testing"
)

// client writes fs/nfsd/clients/<id>/info as kernel patch 0027 shows it, for
// worker-3's client.
func (p *fakeProc) client(id int, readBytes, writeBytes int64) {
	p.write(fmt.Sprintf("fs/nfsd/clients/%d/info", id), fmt.Sprintf(
		"clientid: 0x%x\naddress: %q\nstatus: confirmed\nseconds from last renew: 3\n"+
			"name: %q\nminor version: 2\ncallback state: UP\nadmin-revoked states: 0\n"+
			"pnfs mds read bytes: %d\npnfs mds write bytes: %d\nsession slots: 64\n",
		0x6a00+id, worker3Address, worker3Name, readBytes, writeBytes))
}

func newMDSDataWatch(p *fakeProc, rec *recorder) *MDSDataWatch {
	return &MDSDataWatch{ProcRoot: p.root, Log: rec}
}

const (
	worker3Address = "10.0.0.13:739"
	worker3Name    = "Linux NFSv4.2 worker-3.ocp.simplyblock.ai"
)

// Regression: SFAM-2850. One client sent 6.8 GB through the metadata server in
// 11 minutes, for the rest of a run, and nothing on the server said which.
func TestAClientSendingDataThroughTheServerIsNamed(t *testing.T) {
	p := newFakeProc(t)
	rec := &recorder{}
	w := newMDSDataWatch(p, rec)
	p.client(5, 1<<30, 1<<30)
	w.Tick()

	p.client(5, 1<<30+400<<20, 1<<30+200<<20)
	w.Tick()

	if len(rec.warnings) != 1 {
		t.Fatalf("warnings = %q, want one naming the client", rec.warnings)
	}
	for _, want := range []string{worker3Address, "worker-3.ocp.simplyblock.ai", "419430400", "209715200"} {
		if !strings.Contains(rec.warnings[0], want) {
			t.Errorf("warning %q does not carry %q", rec.warnings[0], want)
		}
	}
}

// What the server carried before the agent first looked is not named: the
// agent may have restarted long after it, and only growth says anything now.
func TestCountsAtTheFirstLookAreNotNamed(t *testing.T) {
	p := newFakeProc(t)
	rec := &recorder{}
	p.client(5, 1<<30, 1<<30)
	newMDSDataWatch(p, rec).Tick()
	if len(rec.warnings) != 0 {
		t.Errorf("warnings = %q at the first look, want none", rec.warnings)
	}
}

// A probe's block and the data that goes through the server by design stay
// under the threshold.
func TestAClientBelowTheThresholdIsNotNamed(t *testing.T) {
	p := newFakeProc(t)
	rec := &recorder{}
	w := newMDSDataWatch(p, rec)
	p.client(5, 0, 0)
	w.Tick()
	p.client(5, 4096, mdsDataThreshold-4097)
	w.Tick()
	if len(rec.warnings) != 0 {
		t.Errorf("warnings = %q below the threshold, want none", rec.warnings)
	}
}

// A client that appears between two looks is counted from zero: it connected
// after the last look, so all of its count is new.
func TestAClientNewSinceTheLastLookIsCountedFromZero(t *testing.T) {
	p := newFakeProc(t)
	rec := &recorder{}
	w := newMDSDataWatch(p, rec)
	w.Tick()
	p.client(9, 0, 64<<20)
	w.Tick()
	if len(rec.warnings) != 1 || !strings.Contains(rec.warnings[0], worker3Address) {
		t.Errorf("warnings = %q, want one naming the new client", rec.warnings)
	}
}

// A guest kernel without the counters, or with nfsd stopped, is not an error
// and names nobody.
func TestAKernelWithoutTheCountersNamesNobody(t *testing.T) {
	p := newFakeProc(t)
	rec := &recorder{}
	w := newMDSDataWatch(p, rec)
	p.write("fs/nfsd/clients/5/info", "clientid: 0x6a05\naddress: \"10.0.0.13:739\"\n")
	w.Tick()
	w.Tick()
	empty := newMDSDataWatch(newFakeProc(t), rec)
	empty.Tick()
	empty.Tick()
	if all := rec.all(); all != "" {
		t.Errorf("logged %q, want nothing", all)
	}
}
