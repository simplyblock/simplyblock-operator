// The choice of a node's address from the network's pool. The functions are
// pure: what is held and what was turned away come in as arguments.
//
// Specified by operator/docs/designs/design-storage-network.md §4.5.

package node

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

var (
	// errPoolExhausted means every address of the pool is held, excluded, or was
	// turned away.
	errPoolExhausted = errors.New("the address pool has no address left")

	// errPoolInvalid means the pool is not a range inside its subnet, or an exclude
	// entry is not an address or a range.
	errPoolInvalid = errors.New("the address pool is not a range inside its subnet")
)

// candidateBatch is how many addresses beyond the assigned one the probe checks
// in a pass, so that a subnet with a busy start costs one probe and not one per
// address.
const candidateBatch = 16

// allocateAddress returns the first address of the pool that is not in held, not
// in rejected, and not excluded, written with the prefix length of the pool's
// subnet.
func allocateAddress(pool simplyblockv1alpha2.NetworkAddressPool, held, rejected []string) (string, error) {
	candidates, err := candidateAddresses(pool, held, rejected, 1)
	if err != nil {
		return "", err
	}
	return candidates[0], nil
}

// candidateAddresses returns up to limit pool addresses in order, with the
// subnet's prefix length, skipping held, turned-away, and excluded ones. The
// network and broadcast addresses are skipped except in a /31 or /32. Addresses
// compare by host part. It is errPoolExhausted when nothing is left.
func candidateAddresses(
	pool simplyblockv1alpha2.NetworkAddressPool, held, rejected []string, limit int,
) ([]string, error) {
	subnet, err := netip.ParsePrefix(pool.Subnet)
	if err != nil {
		return nil, fmt.Errorf("%w: subnet %q: %v", errPoolInvalid, pool.Subnet, err)
	}
	first, last, err := rangeOf(pool, subnet)
	if err != nil {
		return nil, err
	}
	excluded, err := parseExcludes(pool.Exclude)
	if err != nil {
		return nil, err
	}

	taken := map[netip.Addr]bool{}
	for _, a := range append(append([]string{}, held...), rejected...) {
		if host, ok := hostOf(a); ok {
			taken[host] = true
		}
	}

	keepBack := subnet.Bits() < 31
	network, broadcast := subnet.Masked().Addr(), lastAddress(subnet)
	var found []string
	for a := first; a.Compare(last) <= 0 && len(found) < limit; a = a.Next() {
		if keepBack && (a == network || a == broadcast) || taken[a] || excluded.contains(a) {
			continue
		}
		found = append(found, netip.PrefixFrom(a, subnet.Bits()).String())
		if a == last {
			break
		}
	}
	if len(found) == 0 {
		return nil, errPoolExhausted
	}
	return found, nil
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

// excludedRanges are the pool's exclude entries as inclusive address ranges.
type excludedRanges [][2]netip.Addr

func (e excludedRanges) contains(a netip.Addr) bool {
	for _, r := range e {
		if a.Compare(r[0]) >= 0 && a.Compare(r[1]) <= 0 {
			return true
		}
	}
	return false
}

// parseExcludes reads each entry as an address or as a range of two addresses
// joined by a hyphen. An entry that is neither, or a range that runs backward,
// makes the pool invalid. An entry outside the subnet is kept, and matches
// nothing the pool hands out.
func parseExcludes(entries []string) (excludedRanges, error) {
	ranges := make(excludedRanges, 0, len(entries))
	for _, entry := range entries {
		from, to, isRange := strings.Cut(entry, "-")
		start, errStart := netip.ParseAddr(from)
		end := start
		var errEnd error
		if isRange {
			end, errEnd = netip.ParseAddr(to)
		}
		if errStart != nil || errEnd != nil || !start.Is4() || !end.Is4() || end.Less(start) {
			return nil, fmt.Errorf("%w: exclude entry %q is not an IPv4 address or range", errPoolInvalid, entry)
		}
		ranges = append(ranges, [2]netip.Addr{start, end})
	}
	return ranges, nil
}

// lastAddress is the highest address of the subnet, its broadcast address.
func lastAddress(subnet netip.Prefix) netip.Addr {
	bytes := subnet.Masked().Addr().As4()
	for bit := subnet.Bits(); bit < 32; bit++ {
		bytes[bit/8] |= 1 << (7 - bit%8)
	}
	return netip.AddrFrom4(bytes)
}
