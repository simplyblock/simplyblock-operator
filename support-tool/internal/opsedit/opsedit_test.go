// The discovery-run editor: its prefill with and without a bundle, the run it
// renders, and the checks admission and the operator's webhook make.

package opsedit

import (
	"bytes"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/bundletest"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

func TestThePrefillWithoutABundleIsADefaultRun(t *testing.T) {
	req := Prefill(nil)
	if req.Name == "" || req.Namespace != "simplyblock" {
		t.Errorf("the prefill is %+v", req)
	}
}

func TestThePrefillFromABundleTakesItsRunsSettingsUnderANewName(t *testing.T) {
	b, err := supportbundle.Read(bytes.NewReader(bundletest.Archive(t, bundletest.Fixture(t, "bundle-v10"),
		func(name string, data []byte) ([]byte, bool) {
			if name == "operatorops.yaml" {
				return bytes.Replace(data, []byte("  action: Discover"),
					[]byte("  action: Discover\n  discover:\n    enableLogicalBlockDevices: true"), 1), true
			}
			return data, true
		})))
	if err != nil {
		t.Fatal(err)
	}
	req := Prefill(b)
	if req.Name == "" || req.Name == b.Run.Name || req.Namespace != b.Run.Namespace {
		t.Errorf("the prefill is named %s/%s, beside the bundle's %s", req.Namespace, req.Name, b.Run.Name)
	}
	if v := req.Discover.EnableLogicalBlockDevices; v == nil || !*v {
		t.Errorf("the prefill dropped the run's settings: %+v", req.Discover)
	}
}

func TestTheRunRendersAsAnOperatorOpsToApply(t *testing.T) {
	ops, err := Build(Request{Name: "discovery-2", Namespace: "simplyblock",
		Discover: simplyblockv1alpha2.DiscoverSpec{EnablePartitionedDevices: ptr.To(true)}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	out, err := Render(ops)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("the rendering does not parse: %v", err)
	}
	spec, _ := doc["spec"].(map[string]any)
	if doc["kind"] != "OperatorOps" || spec["action"] != "Discover" || spec["discover"] == nil {
		t.Errorf("the rendering is %s", out)
	}
	if _, ok := doc["status"]; ok {
		t.Error("the rendering carries a status")
	}
}

func TestARunWithoutAValidNameIsRefused(t *testing.T) {
	if _, err := Build(Request{Name: "Not A Name", Namespace: "simplyblock"}); err == nil {
		t.Error("a run named \"Not A Name\" was built")
	}
}

func TestTheChecksAreAdmissionsAndTheWebhooks(t *testing.T) {
	ops, err := Build(Request{Name: "discovery-2", Namespace: "simplyblock", Discover: simplyblockv1alpha2.DiscoverSpec{
		Workers:      []string{"worker-1"},
		NodeSelector: map[string]string{"role": "storage"},
		DeviceFilter: &simplyblockv1alpha2.DeviceFilter{DriveSizeRange: "4T-100G"},
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	found := Check(ops)
	var schema, webhook bool
	for _, f := range found {
		schema = schema || (f.Source == "admission" && strings.Contains(f.Message, "state one or the other"))
		webhook = webhook || (f.Source == "webhook" && strings.Contains(f.Message, "4T-100G"))
	}
	if !schema || !webhook {
		t.Errorf("the findings are %+v, want both the schema's and the webhook's", found)
	}

	ok, err := Build(Request{Name: "discovery-3", Namespace: "simplyblock"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if found := Check(ok); len(found) != 0 {
		t.Errorf("a default run is refused: %+v", found)
	}
}

func TestTheNextRunCountsUp(t *testing.T) {
	for name, want := range map[string]string{
		"discovery":         "discovery-2",
		"discovery-2":       "discovery-3",
		"discover-fleet-02": "discover-fleet-03",
		"discover-fleet-09": "discover-fleet-10",
		"initial-discovery": "initial-discovery-2",
	} {
		if got := nextName(name); got != want {
			t.Errorf("the run after %s is %s, want %s", name, got, want)
		}
	}
}
