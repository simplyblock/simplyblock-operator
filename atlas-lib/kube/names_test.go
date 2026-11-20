// Tests for the naming rules the operator and the CSI driver have to spell
// alike, checked against the validators the API server itself uses.
package kube

import (
	"net/netip"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// TestPoolNodeLabelKey test to make sure that `PoolNodeLabelKey` doesn't generate label key more 63 bits
func TestPoolNodeLabelKey(t *testing.T) {
	const uuid = "432ef113-aa65-46a0-9210-67d92cb955e4"

	key := PoolNodeLabelKey(uuid)
	if errs := validation.IsQualifiedName(key); len(errs) != 0 {
		t.Fatalf("PoolNodeLabelKey(%q) = %q, not a valid label name: %s",
			uuid, key, strings.Join(errs, "; "))
	}
	if !strings.HasPrefix(key, LabelPoolPrefix) {
		t.Errorf("PoolNodeLabelKey(%q) = %q, want the %q prefix the CSI node plugin scans for",
			uuid, key, LabelPoolPrefix)
	}
	if key == PoolNodeLabelKey("9c1e4d7a-3f52-4b18-8a6d-2e7f0b5c4a91") {
		t.Errorf("two pools share the key %q, so a node in both cannot be told apart", key)
	}
	if errs := validation.IsValidLabelValue(LabelPoolAllowed); len(errs) != 0 {
		t.Errorf("LabelPoolAllowed = %q is not a valid label value: %s",
			LabelPoolAllowed, strings.Join(errs, "; "))
	}
}

// TestSubnetLabelRoundTrips checks that the label the CSI node plugin publishes
// for a subnet is a valid label name and that the operator reads back the same
// subnet from it, network address and prefix length both.
func TestSubnetLabelRoundTrips(t *testing.T) {
	for _, cidr := range []string{"10.10.10.0/24", "192.168.0.0/16", "172.16.4.128/25"} {
		subnet := netip.MustParsePrefix(cidr)
		key := SubnetLabel(subnet)
		if errs := validation.IsQualifiedName(key); len(errs) != 0 {
			t.Fatalf("SubnetLabel(%s) = %q, not a valid label name: %s", cidr, key, strings.Join(errs, "; "))
		}
		if !strings.HasPrefix(key, LabelSubnetPrefix) {
			t.Errorf("SubnetLabel(%s) = %q, want the %q prefix", cidr, key, LabelSubnetPrefix)
		}
		back, ok := SubnetOfLabel(key)
		if !ok || back != subnet {
			t.Errorf("SubnetOfLabel(%q) = %s, %t, want %s", key, back, ok, cidr)
		}
	}
}

// TestSubnetLabelNamesTheNetworkNotTheHost checks that two hosts on one subnet
// publish the same label, so the operator can match them against one range.
func TestSubnetLabelNamesTheNetworkNotTheHost(t *testing.T) {
	a := SubnetLabel(netip.MustParsePrefix("10.10.10.244/24"))
	b := SubnetLabel(netip.MustParsePrefix("10.10.10.247/24"))
	if a != b || a != SubnetLabel(netip.MustParsePrefix("10.10.10.0/24")) {
		t.Errorf("hosts on 10.10.10.0/24 publish %q and %q, want one label for the subnet", a, b)
	}
}

// TestSubnetOfLabelRefusesOtherLabels checks that labels outside the prefix,
// and malformed ones inside it, are not read as subnets.
func TestSubnetOfLabelRefusesOtherLabels(t *testing.T) {
	for _, key := range []string{
		LabelKVMCapable,
		LabelSubnetPrefix + "10.10.10.0",
		LabelSubnetPrefix + "10.10.10.0_33",
		LabelSubnetPrefix + "fd00__64",
		LabelSubnetPrefix + "10.10.10.5_24",
	} {
		if subnet, ok := SubnetOfLabel(key); ok {
			t.Errorf("SubnetOfLabel(%q) = %s, want no subnet", key, subnet)
		}
	}
}
