// The pool a node's address on a network is taken from: the first address of the
// range that no other node holds and that was not turned away before, with the
// subnet's prefix length.

package node

import (
	"errors"
	"slices"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func poolOf(subnet, first, last string) simplyblockv1alpha2.NetworkAddressPool {
	return simplyblockv1alpha2.NetworkAddressPool{Subnet: subnet, First: first, Last: last}
}

// allocateAddress takes the first address that is not held, turned away, or
// excluded. A held address matches by its host part, because two nodes that
// state one address with different prefix lengths still clash on the wire.
func TestAllocateAddressSkipsWhatIsTaken(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pool         simplyblockv1alpha2.NetworkAddressPool
		held, turned []string
		want         string
	}{
		{"nothing is held", poolOf("10.10.10.0/24", "10.10.10.101", "10.10.10.103"), nil, nil, firstInPool},
		{"another node holds the first", poolOf("10.10.10.0/24", "10.10.10.101", "10.10.10.103"), []string{firstInPool}, nil, nextInPool},
		{"a held address with another prefix length", poolOf("10.10.10.0/24", "10.10.10.101", "10.10.10.103"), []string{"10.10.10.101/16"}, nil, nextInPool},
		{"the node was turned away from two", poolOf("10.10.10.0/24", "10.10.10.101", "10.10.10.103"), nil, []string{firstInPool, nextInPool}, "10.10.10.103/24"},
		{"the subnet's prefix length is the address's", poolOf("10.10.0.0/16", "10.10.1.5", "10.10.1.9"), nil, nil, "10.10.1.5/16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := allocateAddress(tc.pool, tc.held, tc.turned)
			if err != nil || got != tc.want {
				t.Fatalf("allocateAddress = %q, %v, want %s", got, err, tc.want)
			}
		})
	}
}

func TestAPoolWithNothingLeftIsExhausted(t *testing.T) {
	_, err := allocateAddress(poolOf("10.10.10.0/24", "10.10.10.101", "10.10.10.102"),
		[]string{firstInPool}, []string{nextInPool})
	if !errors.Is(err, errPoolExhausted) {
		t.Fatalf("err = %v, want the pool exhausted", err)
	}
}

// Boundary: a pool of one address hands it out once.
func TestAPoolOfOneAddressIsTakenOnce(t *testing.T) {
	pool := poolOf("10.10.10.0/24", "10.10.10.101", "10.10.10.101")
	if got, err := allocateAddress(pool, nil, nil); err != nil || got != firstInPool {
		t.Fatalf("first allocation = %q, %v", got, err)
	}
	if _, err := allocateAddress(pool, []string{firstInPool}, nil); !errors.Is(err, errPoolExhausted) {
		t.Fatalf("second allocation err = %v, want the pool exhausted", err)
	}
}

// Boundary: the subnet's network and broadcast addresses belong to no host.
func TestTheNetworkAndBroadcastAddressesAreNeverHandedOut(t *testing.T) {
	pool := poolOf("10.10.10.0/24", "10.10.10.254", "10.10.10.255")
	got, err := allocateAddress(pool, nil, nil)
	if err != nil || got != "10.10.10.254/24" {
		t.Fatalf("allocateAddress = %q, %v, want 10.10.10.254/24", got, err)
	}
	if _, err := allocateAddress(pool, []string{"10.10.10.254/24"}, nil); !errors.Is(err, errPoolExhausted) {
		t.Fatalf("err = %v, want the pool exhausted without the broadcast address", err)
	}
	if _, err := allocateAddress(poolOf("10.10.10.0/24", "10.10.10.0", "10.10.10.0"), nil, nil); !errors.Is(err, errPoolExhausted) {
		t.Fatalf("the network address was handed out: err = %v", err)
	}
}

func TestAPoolThatIsNotARangeInItsSubnetIsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool simplyblockv1alpha2.NetworkAddressPool
	}{
		{"first after last", poolOf("10.10.10.0/24", "10.10.10.110", "10.10.10.101")},
		{"first outside the subnet", poolOf("10.10.10.0/24", "10.10.11.5", "10.10.11.9")},
		{"last outside the subnet", poolOf("10.10.10.0/24", "10.10.10.5", "10.10.11.9")},
		{"an unparsable subnet", poolOf("not-a-subnet", "10.10.10.5", "10.10.10.9")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := allocateAddress(tc.pool, nil, nil); !errors.Is(err, errPoolInvalid) {
				t.Fatalf("err = %v, want an invalid pool", err)
			}
		})
	}
}

// The subnet is the one required value. Without first and last the range is the
// whole subnet.
func TestAPoolOfASubnetAloneIsTheWholeSubnet(t *testing.T) {
	pool := simplyblockv1alpha2.NetworkAddressPool{Subnet: "10.10.10.0/24"}

	got, err := candidateAddresses(pool, nil, nil, 3)

	want := []string{"10.10.10.1/24", "10.10.10.2/24", "10.10.10.3/24"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("candidateAddresses = %v, %v, want %v", got, err, want)
	}
}

func TestOnlyOneOfFirstAndLastIsAnInvalidPool(t *testing.T) {
	for _, pool := range []simplyblockv1alpha2.NetworkAddressPool{
		{Subnet: "10.10.10.0/24", First: "10.10.10.5"},
		{Subnet: "10.10.10.0/24", Last: "10.10.10.9"},
	} {
		if _, err := allocateAddress(pool, nil, nil); !errors.Is(err, errPoolInvalid) {
			t.Fatalf("pool %+v: err = %v, want an invalid pool", pool, err)
		}
	}
}

func TestExcludedAddressesAndRangesAreNeverHandedOut(t *testing.T) {
	pool := simplyblockv1alpha2.NetworkAddressPool{
		Subnet:  "10.10.10.0/24",
		Exclude: []string{"10.10.10.1-10.10.10.10", "10.10.10.12", "10.10.10.14-10.10.10.14"},
	}

	got, err := candidateAddresses(pool, nil, nil, 4)

	want := []string{"10.10.10.11/24", "10.10.10.13/24", "10.10.10.15/24", "10.10.10.16/24"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("candidateAddresses = %v, %v, want %v", got, err, want)
	}
}

// An entry that names nothing in the subnet changes nothing, so a list shared by
// several clusters may carry addresses of the others.
func TestAnExcludeEntryOutsideTheSubnetHasNoEffect(t *testing.T) {
	pool := simplyblockv1alpha2.NetworkAddressPool{
		Subnet: "10.10.10.0/24", First: "10.10.10.101", Last: "10.10.10.102",
		Exclude: []string{"192.168.0.1", "172.16.0.1-172.16.0.9"},
	}

	got, err := allocateAddress(pool, nil, nil)
	if err != nil || got != firstInPool {
		t.Fatalf("allocateAddress = %q, %v, want %s", got, err, firstInPool)
	}
}

func TestAnExcludeEntryThatIsNotAnAddressOrARangeIsInvalid(t *testing.T) {
	for _, entry := range []string{"ten", "10.10.10.9-10.10.10.5", "10.10.10.1-", "10.10.10.1-10.10.10.2-10.10.10.3", "10.10.10.1/32", ""} {
		t.Run(entry, func(t *testing.T) {
			pool := simplyblockv1alpha2.NetworkAddressPool{Subnet: "10.10.10.0/24", Exclude: []string{entry}}
			if _, err := allocateAddress(pool, nil, nil); !errors.Is(err, errPoolInvalid) {
				t.Fatalf("err = %v, want an invalid pool", err)
			}
		})
	}
}

func TestAllExcludedIsExhausted(t *testing.T) {
	pool := simplyblockv1alpha2.NetworkAddressPool{
		Subnet: "10.10.10.0/24", Exclude: []string{"10.10.10.0-10.10.10.255"},
	}
	if _, err := allocateAddress(pool, nil, nil); !errors.Is(err, errPoolExhausted) {
		t.Fatalf("err = %v, want the pool exhausted", err)
	}
}

// The batch is what the probe checks in one pass: in order, bounded, and without
// what is held, turned away, or excluded.
func TestTheBatchIsInOrderBoundedAndSkipsWhatIsTaken(t *testing.T) {
	pool := simplyblockv1alpha2.NetworkAddressPool{
		Subnet: "10.10.10.0/24", First: "10.10.10.101", Last: "10.10.10.120", Exclude: []string{"10.10.10.103"},
	}

	got, err := candidateAddresses(pool, []string{"10.10.10.101/24"}, []string{"10.10.10.104/24"}, 3)

	want := []string{"10.10.10.102/24", "10.10.10.105/24", "10.10.10.106/24"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("candidateAddresses = %v, %v, want %v", got, err, want)
	}
}

// Boundary: a /30 has two host addresses, and a /31 and a /32 have no network or
// broadcast address to keep back (RFC 3021).
func TestSmallSubnetsHandOutTheirHostAddresses(t *testing.T) {
	for _, tc := range []struct {
		subnet string
		want   []string
	}{
		{"10.10.10.0/30", []string{"10.10.10.1/30", "10.10.10.2/30"}},
		{"10.10.10.0/31", []string{"10.10.10.0/31", "10.10.10.1/31"}},
		{"10.10.10.7/32", []string{"10.10.10.7/32"}},
	} {
		t.Run(tc.subnet, func(t *testing.T) {
			got, err := candidateAddresses(simplyblockv1alpha2.NetworkAddressPool{Subnet: tc.subnet}, nil, nil, 16)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("candidateAddresses = %v, %v, want %v", got, err, tc.want)
			}
		})
	}
}
