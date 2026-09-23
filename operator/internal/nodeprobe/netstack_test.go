// That the interface kind and the stack around it survive the trip into the
// report, and that a reader refuses the schema that predates them.
//
// The three fields are what make the management-interface rule possible: every
// software interface is virtual, and only the kind separates a bond or a tagged
// VLAN from a veth. A report that carried the readings and dropped these would
// leave the rule with the same collapsed answer it had.

package nodeprobe

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/simplyblock/atlas/inventory"
)

func TestFromInventoryCarriesTheKindAndTheStack(t *testing.T) {
	report := FromInventory("worker-1", time.Now(), inventory.Inventory{
		Interfaces: []inventory.Interface{
			{
				Name: "bond0", Kind: inventory.LinkBond, Virtual: true,
				Lower: []string{"eth0", "eth1"}, Upper: []string{"bond0.100"},
				SpeedMbps: 50000, OperState: inventory.LinkUp,
			},
			{
				Name: "bond0.100", Kind: inventory.LinkVLAN, Virtual: true,
				Lower: []string{"bond0"}, Addresses: []string{"10.0.0.11"},
				OperState: inventory.LinkUp,
			},
			{
				Name: "eth0", Kind: inventory.LinkPhysical,
				Upper: []string{"bond0"}, PCIAddress: "0000:3b:00.0", NUMANode: 0,
			},
		},
	}, nil)

	byName := map[string]Interface{}
	for _, iface := range report.Interfaces {
		byName[iface.Name] = iface
	}

	bond := byName["bond0"]
	if bond.Kind != string(inventory.LinkBond) {
		t.Errorf("the bond reports kind %q, want %q", bond.Kind, inventory.LinkBond)
	}
	if !slices.Equal(bond.Lower, []string{"eth0", "eth1"}) {
		t.Errorf("the bond reports members %v, want both NICs", bond.Lower)
	}
	if !slices.Equal(bond.Upper, []string{"bond0.100"}) {
		t.Errorf("the bond reports %v stacked on it, want the VLAN", bond.Upper)
	}
	if vlan := byName["bond0.100"]; vlan.Kind != string(inventory.LinkVLAN) ||
		!slices.Equal(vlan.Lower, []string{"bond0"}) {
		t.Errorf("the VLAN reads as %+v", vlan)
	}
	if nic := byName["eth0"]; nic.Kind != string(inventory.LinkPhysical) ||
		!slices.Equal(nic.Upper, []string{"bond0"}) {
		t.Errorf("the NIC reads as %+v", nic)
	}
}

func TestTheKindAndTheStackSurviveTheJSON(t *testing.T) {
	// The report is a wire format between a probe pod and the operator, so a
	// field that is not marshaled is a field the discovery step never sees.
	encoded, err := Encode(Report{
		Node: "worker-1",
		Interfaces: []Interface{{
			Name: "bond0", Kind: string(inventory.LinkBond),
			Lower: []string{"eth0", "eth1"}, Upper: []string{"bond0.100"},
		}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Interfaces) != 1 {
		t.Fatalf("decoded %d interfaces", len(decoded.Interfaces))
	}
	iface := decoded.Interfaces[0]
	if iface.Kind != string(inventory.LinkBond) ||
		!slices.Equal(iface.Lower, []string{"eth0", "eth1"}) ||
		!slices.Equal(iface.Upper, []string{"bond0.100"}) {
		t.Errorf("the round trip produced %+v", iface)
	}

	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	first := raw["interfaces"].([]any)[0].(map[string]any)
	for _, key := range []string{"kind", "lower", "upper"} {
		if _, carried := first[key]; !carried {
			t.Errorf("the encoded interface carries no %q", key)
		}
	}
}

func TestAReportFromTheSchemaBeforeTheStackIsRefused(t *testing.T) {
	// A Job keeps the image it started with, so an operator upgraded mid-run
	// reads reports from the previous probe. One written before the kind existed
	// describes its bonds as virtual and nothing else, and reading it as current
	// would put the rule back where it was.
	before := []byte(`{"version":3,"node":"worker-1"}`)

	if _, err := Decode(before); err == nil {
		t.Fatal("a report from the previous schema was accepted")
	}
	if ReportVersion < 4 {
		t.Errorf("the report is version %d, and the kind and the stack arrived at version 4", ReportVersion)
	}
}
