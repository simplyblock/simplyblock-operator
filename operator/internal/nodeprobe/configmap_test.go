// The name a report is written under, and what happens to a report that will
// not fit.
//
// The naming is the part with real cases in it. A node is called worker-3 in a
// laboratory and ip-10-0-1-23.eu-central-1.compute.internal on a cloud, and the
// second of those plus a run name is longer than an object name may be, so the
// name is sanitized and truncated and the labels carry what the name lost.

package nodeprobe

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/inventory"
)

func TestObjectNameIsDeterministicAndLegal(t *testing.T) {
	// Both sides compute the name without telling each other: the probe writes
	// the object and the discovery step reads it back, so the same run and node
	// have to produce the same name every time.
	for _, tc := range []struct{ run, node string }{
		{testRun, testNode},
		{testRun, "ip-10-0-1-23.eu-central-1.compute.internal"},
		{testRun, "WORKER_3"},
		{"a", "b"},
		{strings.Repeat("run", 40), strings.Repeat("node.", 60)},
	} {
		name := ObjectName(tc.run, tc.node)
		if again := ObjectName(tc.run, tc.node); again != name {
			t.Errorf("two calls for (%q, %q) produced %q and %q", tc.run, tc.node, name, again)
		}
		if errs := validation.NameIsDNSSubdomain(name, false); len(errs) > 0 {
			t.Errorf("(%q, %q) produced %q, which is not a legal object name: %v",
				tc.run, tc.node, name, errs)
		}
		if !strings.HasPrefix(name, namePrefix) {
			t.Errorf("%q does not open with %q", name, namePrefix)
		}
	}
}

func TestObjectNameSeparatesNodesThatTruncateTogether(t *testing.T) {
	// Two nodes in one rack differ in the last label of a very long name, so
	// truncation alone would give them one object and lose one of the two
	// reports. The digest is what keeps them apart.
	run := testRun
	long := strings.Repeat("compute-node-with-a-long-name.", 10)

	first := ObjectName(run, long+"worker-3.internal")
	second := ObjectName(run, long+"worker-4.internal")
	if first == second {
		t.Errorf("two nodes whose names truncate to the same stem both produced %q", first)
	}
}

func TestObjectNameChangesWithTheRun(t *testing.T) {
	// A second discovery writes a second document rather than editing the
	// first, and its reports have to be second objects for the same reason.
	if a, b := ObjectName("oops-1", testNode), ObjectName("oops-2", testNode); a == b {
		t.Errorf("two runs against one node both wrote %q", a)
	}
}

func TestConfigMapCarriesTheReportAndTheLabelsToFindItBy(t *testing.T) {
	report := FromInventory(testNode, probedAt, minimalInventory(), nil)

	cm, err := ConfigMap("simplyblock", testRun, nil, report)
	if err != nil {
		t.Fatalf("render the ConfigMap: %v", err)
	}

	if cm.Name != ObjectName(testRun, testNode) {
		t.Errorf("the object is called %q", cm.Name)
	}
	if cm.Namespace != "simplyblock" {
		t.Errorf("the object is in %q, want simplyblock", cm.Namespace)
	}
	for key, want := range ReportSelector(testRun) {
		if cm.Labels[key] != want {
			t.Errorf("the label %s is %q, want %q, so a run's reports can be listed",
				key, cm.Labels[key], want)
		}
	}
	if cm.Labels[LabelNode] != testNode {
		t.Errorf("the node label is %q, want testNode", cm.Labels[LabelNode])
	}

	readBack, err := ReportFromConfigMap(cm)
	if err != nil {
		t.Fatalf("read the report back: %v", err)
	}
	if readBack.Node != testNode || len(readBack.Devices) != len(report.Devices) {
		t.Errorf("read back %+v", readBack)
	}
}

func TestConfigMapStampsTheOwnerItWasGiven(t *testing.T) {
	// The owner is how a run's reports are collected when the run is deleted,
	// and it is passed in rather than looked up so the probe needs no read
	// access to the object that owns it.
	owner := &metav1.OwnerReference{
		APIVersion: "storage.simplyblock.io/v1alpha1",
		Kind:       "OperatorOps",
		Name:       testRun,
		UID:        "8f14e45f-ceea-467a-9d1f-2e0b1c4b6b8a",
	}

	cm, err := ConfigMap("simplyblock", testRun, owner, FromInventory(testNode, probedAt, minimalInventory(), nil))
	if err != nil {
		t.Fatalf("render the ConfigMap: %v", err)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != owner.UID {
		t.Errorf("the owner references are %+v, want the run", cm.OwnerReferences)
	}
}

func TestConfigMapRefusesWhatItCannotDescribe(t *testing.T) {
	report := FromInventory(testNode, probedAt, minimalInventory(), nil)

	for _, tc := range []struct {
		name           string
		namespace, run string
		node           string
	}{
		{"no namespace", "", "oops-1", testNode},
		{"no run", "simplyblock", "", testNode},
		{"no node", "simplyblock", "oops-1", ""},
	} {
		r := report
		r.Node = tc.node
		if _, err := ConfigMap(tc.namespace, tc.run, nil, r); err == nil {
			t.Errorf("%s: rendered a ConfigMap anyway", tc.name)
		}
	}
}

func TestReportFromConfigMapRefusesOneWithNothingInIt(t *testing.T) {
	// A ConfigMap the probe created and did not finish writing has no report
	// key. Reading it as an empty report would be reading a worker with no
	// devices, which is a worker nothing can be deployed on rather than a
	// worker that was never asked.
	empty := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "sb-nodeprobe-x", Namespace: "simplyblock"}}

	if _, err := ReportFromConfigMap(empty); err == nil {
		t.Error("read a report out of a ConfigMap that carries none")
	}
	if _, err := ReportFromConfigMap(nil); err == nil {
		t.Error("read a report out of no ConfigMap at all")
	}
}

func TestReportFromConfigMapRefusesAVersionItDoesNotKnow(t *testing.T) {
	// A Job keeps the image it started with, so an operator upgraded mid-run
	// reads reports written by the previous probe. A field that changed meaning
	// between the two would be read as the current one, so the version is
	// checked rather than trusted.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-nodeprobe-x", Namespace: "simplyblock"},
		Data:       map[string]string{ReportKey: `{"version":99,"node":testNode}`},
	}

	if _, err := ReportFromConfigMap(cm); err == nil {
		t.Error("read a report written by a schema this operator does not know")
	}
}

func TestDecodeRefusesAReportThatNamesNoNode(t *testing.T) {
	// Everything in a report is attributed to a worker, and one that names none
	// cannot be attributed at all.
	if _, err := Decode([]byte(`{"version":1}`)); err == nil {
		t.Error("decoded a report that names no node")
	}
}

func TestLabelValueFitsWhatALabelHolds(t *testing.T) {
	// A node name may be 253 characters and a label value 63, which is why the
	// labels are for selecting and the report's own data says which node it is
	// about.
	long := strings.Repeat("compute.node.", 30) + testNode

	value := labelValue(long)
	if errs := utilvalidation.IsValidLabelValue(value); len(errs) > 0 {
		t.Errorf("labelValue produced %q, which a label may not hold: %v", value, errs)
	}
}

// minimalInventory is a worker with one free disk, which is enough for a report
// with something in it.
func minimalInventory() inventory.Inventory {
	return inventory.Inventory{
		CPU:     inventory.CPU{OnlineCount: 16, PhysicalCores: 8},
		Devices: []blockdev.Candidate{oneFreeDisk()},
	}
}
