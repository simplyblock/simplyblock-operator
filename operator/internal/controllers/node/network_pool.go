// The choice of a node's address from the network's pool. The function is pure:
// what other nodes hold comes in as an argument.
//
// Specified by operator/docs/designs/design-storage-network.md §4.5.

package node

import (
	"errors"
	"fmt"
	"net/netip"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

var (
	// errPoolExhausted means every address of the pool is held.
	errPoolExhausted = errors.New("the address pool has no address left")

	// errPoolInvalid means the pool is not a range inside its subnet.
	errPoolInvalid = errors.New("the address pool is not a range inside its subnet")
)

// allocateAddress returns the first address of the pool that is not in held,
// written with the prefix length of the pool's subnet. The network and broadcast
// addresses are skipped except in a /31 or /32. Addresses compare by host part.
func allocateAddress(pool simplyblockv1alpha2.NetworkAddressPool, held []string) (string, error) {
	subnet, err := netip.ParsePrefix(pool.Subnet)
	if err != nil {
		return "", fmt.Errorf("%w: subnet %q: %v", errPoolInvalid, pool.Subnet, err)
	}
	first, last, err := rangeOf(pool, subnet)
	if err != nil {
		return "", err
	}

	taken := map[netip.Addr]bool{}
	for _, a := range held {
		if host, ok := hostOf(a); ok {
			taken[host] = true
		}
	}

	keepBack := subnet.Bits() < 31
	network, broadcast := subnet.Masked().Addr(), lastAddress(subnet)
	for a := first; a.Compare(last) <= 0; a = a.Next() {
		if (!keepBack || a != network && a != broadcast) && !taken[a] {
			return netip.PrefixFrom(a, subnet.Bits()).String(), nil
		}
		if a == last {
			break
		}
	}
	return "", errPoolExhausted
}

// rangeOf is the pool's first and last address: the ones it sets, or the whole
// subnet when it sets neither. Setting only one is invalid.
func rangeOf(pool simplyblockv1alpha2.NetworkAddressPool, subnet netip.Prefix) (first, last netip.Addr, err error) {
	if pool.First == "" && pool.Last == "" {
		return subnet.Masked().Addr(), lastAddress(subnet), nil
	}
	first, errFirst := netip.ParseAddr(pool.First)
	last, errLast := netip.ParseAddr(pool.Last)
	if errFirst != nil || errLast != nil || !subnet.Contains(first) || !subnet.Contains(last) || last.Less(first) {
		return first, last, fmt.Errorf("%w: %s to %s in %s", errPoolInvalid, pool.First, pool.Last, pool.Subnet)
	}
	return first, last, nil
}

// lastAddress is the highest address of the subnet, its broadcast address.
func lastAddress(subnet netip.Prefix) netip.Addr {
	bytes := subnet.Masked().Addr().As4()
	for bit := subnet.Bits(); bit < 32; bit++ {
		bytes[bit/8] |= 1 << (7 - bit%8)
	}
	return netip.AddrFrom4(bytes)
}
