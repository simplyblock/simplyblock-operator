// The pool a node's address on a network is taken from: the first address of the
// range that no other node holds, with the subnet's prefix length.

package node

import (
	"errors"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	firstInPool = "10.10.10.101/24"
	nextInPool  = "10.10.10.102/24"
)

func poolOf(subnet, first, last string) simplyblockv1alpha2.NetworkAddressPool {
	return simplyblockv1alpha2.NetworkAddressPool{Subnet: subnet, First: first, Last: last}
}

// allocateAddress takes the first address that is not held. A held address
// matches by its host part, because two nodes that state one address with
// different prefix lengths still clash on the wire.
func TestAllocateAddressSkipsWhatIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool simplyblockv1alpha2.NetworkAddressPool
		held []string
		want string
	}{
		{"nothing is held", poolOf("10.10.10.0/24", "10.10.10.101", "10.10.10.103"), nil, firstInPool},
		{"another node holds the first", poolOf("10.10.10.0/24", "10.10.10.101", "10.10.10.103"), []string{firstInPool}, nextInPool},
		{"a held address with another prefix length", poolOf("10.10.10.0/24", "10.10.10.101", "10.10.10.103"), []string{"10.10.10.101/16"}, nextInPool},
		{"the subnet's prefix length is the address's", poolOf("10.10.0.0/16", "10.10.1.5", "10.10.1.9"), nil, "10.10.1.5/16"},
		{"a subnet alone is the whole subnet", poolOf("10.10.10.0/24", "", ""), nil, "10.10.10.1/24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := allocateAddress(tc.pool, tc.held)
			if err != nil || got != tc.want {
				t.Fatalf("allocateAddress = %q, %v, want %s", got, err, tc.want)
			}
		})
	}
}

func TestAPoolWithNothingLeftIsExhausted(t *testing.T) {
	_, err := allocateAddress(poolOf("10.10.10.0/24", "10.10.10.101", "10.10.10.102"),
		[]string{firstInPool, nextInPool})
	if !errors.Is(err, errPoolExhausted) {
		t.Fatalf("err = %v, want the pool exhausted", err)
	}
}

// Boundary: a pool of one address hands it out once.
func TestAPoolOfOneAddressIsTakenOnce(t *testing.T) {
	pool := poolOf("10.10.10.0/24", "10.10.10.101", "10.10.10.101")
	if got, err := allocateAddress(pool, nil); err != nil || got != firstInPool {
		t.Fatalf("first allocation = %q, %v", got, err)
	}
	if _, err := allocateAddress(pool, []string{firstInPool}); !errors.Is(err, errPoolExhausted) {
		t.Fatalf("second allocation err = %v, want the pool exhausted", err)
	}
}

// Boundary: the subnet's network and broadcast addresses belong to no host.
func TestTheNetworkAndBroadcastAddressesAreNeverHandedOut(t *testing.T) {
	pool := poolOf("10.10.10.0/24", "10.10.10.254", "10.10.10.255")
	if got, err := allocateAddress(pool, nil); err != nil || got != "10.10.10.254/24" {
		t.Fatalf("allocateAddress = %q, %v, want 10.10.10.254/24", got, err)
	}
	if _, err := allocateAddress(pool, []string{"10.10.10.254/24"}); !errors.Is(err, errPoolExhausted) {
		t.Fatalf("err = %v, want the pool exhausted without the broadcast address", err)
	}
	if _, err := allocateAddress(poolOf("10.10.10.0/24", "10.10.10.0", "10.10.10.0"), nil); !errors.Is(err, errPoolExhausted) {
		t.Fatalf("the network address was handed out: err = %v", err)
	}
}

// Boundary: a /31 and a /32 have no network or broadcast address to keep back.
func TestSmallSubnetsHandOutTheirHostAddresses(t *testing.T) {
	for _, tc := range []struct{ subnet, want string }{
		{"10.10.10.0/30", "10.10.10.1/30"},
		{"10.10.10.0/31", "10.10.10.0/31"},
		{"10.10.10.7/32", "10.10.10.7/32"},
	} {
		t.Run(tc.subnet, func(t *testing.T) {
			got, err := allocateAddress(poolOf(tc.subnet, "", ""), nil)
			if err != nil || got != tc.want {
				t.Fatalf("allocateAddress = %q, %v, want %s", got, err, tc.want)
			}
		})
	}
}

func TestAPoolThatIsNotARangeInItsSubnetIsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool simplyblockv1alpha2.NetworkAddressPool
	}{
		{"first after last", poolOf("10.10.10.0/24", "10.10.10.110", "10.10.10.101")},
		{"first outside the subnet", poolOf("10.10.10.0/24", "10.10.11.5", "10.10.11.9")},
		{"an unparsable subnet", poolOf("not-a-subnet", "10.10.10.5", "10.10.10.9")},
		{"only first", poolOf("10.10.10.0/24", "10.10.10.5", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := allocateAddress(tc.pool, nil); !errors.Is(err, errPoolInvalid) {
				t.Fatalf("err = %v, want an invalid pool", err)
			}
		})
	}
}
