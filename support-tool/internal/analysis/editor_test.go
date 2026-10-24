// The checks the editor runs on a document it is building: which stripes the
// chosen workers and devices carry, and whether a journal device is ambiguous.

package analysis

import (
	"bytes"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/bundletest"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

func fleetBundle(t *testing.T) *supportbundle.Bundle {
	t.Helper()
	b, err := supportbundle.Read(bytes.NewReader(bundletest.Archive(t, bundletest.Fixture(t, "bundle-v10"), nil)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheStripeOptionsFollowTheFleet(t *testing.T) {
	b := fleetBundle(t)
	options := StripeOptions(b, b.CDC.DeepCopy())

	byName := map[string]StripeOption{}
	for _, o := range options {
		byName[o.Name] = o
	}
	// Three workers carry 1+1: two to place a stripe on and a spare to
	// rebuild onto. 2+1 needs four, which the operator says.
	if o, ok := byName["1+1"]; !ok || !o.Valid {
		t.Errorf("1+1 on three workers is %+v", o)
	}
	for _, name := range []string{"2+1", "4+2"} {
		o, ok := byName[name]
		if !ok || o.Valid || len(o.Reasons) == 0 {
			t.Errorf("%s on three workers is %+v, want refused with the operator's reason", name, o)
		}
	}
}

func TestAGrowthDocumentOffersNoStripe(t *testing.T) {
	b := fleetBundle(t)
	growth := b.CDC.DeepCopy()
	growth.Spec.Cluster = nil
	growth.Spec.ClusterRef = "existing"
	if options := StripeOptions(b, growth); len(options) != 0 {
		t.Errorf("a document growing a cluster is offered %d stripes", len(options))
	}
}

func TestTiedSmallestDevicesAreReportedForAJournal(t *testing.T) {
	b := fleetBundle(t)
	doc := b.CDC.DeepCopy()
	doc.Spec.Cluster.EnableJournalDevice = ptr.To(true)
	doc.Spec.NodeSets[0].Groups[0].Devices = &simplyblockv1alpha2.DeviceSelection{
		Block: []string{"/dev/nvme0n1", "/dev/nvme1n1", "/dev/nvme2n1"},
	}

	ties := JournalTies(b, doc)
	if len(ties) != len(doc.Spec.NodeSets[0].Groups[0].Workers) {
		t.Fatalf("%d tie findings for %d workers of equal disks: %+v", len(ties), len(doc.Spec.NodeSets[0].Groups[0].Workers), ties)
	}
	if !strings.Contains(ties[0].Message, "worker-01") || ties[0].Severity != SeverityWarning {
		t.Errorf("the finding is %+v", ties[0])
	}

	doc.Spec.Cluster.EnableJournalDevice = nil
	if ties := JournalTies(b, doc); len(ties) != 0 {
		t.Errorf("a document without a journal device has tie findings: %+v", ties)
	}
}
