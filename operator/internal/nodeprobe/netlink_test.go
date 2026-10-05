// That a VLAN's tag and a VXLAN's network identifier survive the trip into the
// report and through its JSON.
//
// The report is the only thing the operator sees of a worker, so an identifier
// the inventory read and the report dropped is one no discovery step can use.

package nodeprobe

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/simplyblock/atlas/inventory"
)

func TestFromInventoryCarriesTheTagAndTheNetworkIdentifier(t *testing.T) {
	report := FromInventory("worker-1", time.Now(), inventory.Inventory{
		Interfaces: []inventory.Interface{
			{
				Name: "bond0.100", Kind: inventory.LinkVLAN, Virtual: true,
				VLAN: &inventory.VLANTag{ID: 100, Protocol: "802.1Q"},
			},
			{
				Name: "vxlan0", Kind: inventory.LinkVXLAN, Virtual: true,
				VXLAN: &inventory.VXLANOverlay{VNI: 4242},
			},
			{Name: "eth0", Kind: inventory.LinkPhysical},
		},
	}, nil)

	byName := map[string]Interface{}
	for _, iface := range report.Interfaces {
		byName[iface.Name] = iface
	}

	if vlan := byName["bond0.100"].VLAN; vlan == nil || vlan.ID != 100 || vlan.Protocol != "802.1Q" {
		t.Errorf("the VLAN reports the tag %+v, want 100 over 802.1Q", vlan)
	}
	if vxlan := byName["vxlan0"].VXLAN; vxlan == nil || vxlan.VNI != 4242 {
		t.Errorf("the overlay reports %+v, want VNI 4242", vxlan)
	}
	if nic := byName["eth0"]; nic.VLAN != nil || nic.VXLAN != nil {
		t.Errorf("the NIC reports a tag %+v or an overlay identity %+v", nic.VLAN, nic.VXLAN)
	}
}

func TestTheTagAndTheNetworkIdentifierSurviveTheJSON(t *testing.T) {
	encoded, err := Encode(Report{
		Node: "worker-1",
		Interfaces: []Interface{
			{Name: "bond0.100", Kind: string(inventory.LinkVLAN), VLAN: &VLAN{ID: 100, Protocol: "802.1Q"}},
			{Name: "vxlan0", Kind: string(inventory.LinkVXLAN), VXLAN: &VXLAN{VNI: 4242}},
			{Name: "eth0", Kind: string(inventory.LinkPhysical)},
		},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := decoded.Interfaces[0].VLAN; got == nil || *got != (VLAN{ID: 100, Protocol: "802.1Q"}) {
		t.Errorf("the VLAN decoded as %+v", got)
	}
	if got := decoded.Interfaces[1].VXLAN; got == nil || got.VNI != 4242 {
		t.Errorf("the overlay decoded as %+v", got)
	}

	var raw struct {
		Interfaces []map[string]any `json:"interfaces"`
	}
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, carried := raw.Interfaces[0]["vlan"]; !carried {
		t.Error("the encoded VLAN carries no \"vlan\"")
	}
	if _, carried := raw.Interfaces[1]["vxlan"]; !carried {
		t.Error("the encoded overlay carries no \"vxlan\"")
	}
	// An interface that is neither carries neither key, so a report from a host
	// with no tagged network reads exactly as it did before the fields existed.
	for _, key := range []string{"vlan", "vxlan"} {
		if _, carried := raw.Interfaces[2][key]; carried {
			t.Errorf("the encoded NIC carries %q", key)
		}
	}
}
