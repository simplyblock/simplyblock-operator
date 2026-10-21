// The node addresses the proxy connects to and the runner redirects: the
// node's own IPv4 addresses, without loopback and link-local.

package hostpath

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
)

// Usable keeps the IPv4 addresses a storage node can listen on and the guest
// can be redirected for, sorted and without duplicates. Loopback is never a
// storage node's listener, and the proxy must not open services bound to it. A
// link-local address could be the guest's own bridge, which a redirect would
// cut off.
func Usable(addrs []netip.Addr) []netip.Addr {
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		a = a.Unmap()
		if !a.Is4() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() {
			continue
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(x, y netip.Addr) int { return x.Compare(y) })
	return slices.Compact(out)
}

// LocalAddrs returns the usable addresses of the interfaces in the current
// network namespace, which for the proxy is the host's.
func LocalAddrs() ([]netip.Addr, error) {
	ifaddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("listing the node's addresses: %w", err)
	}
	addrs := make([]netip.Addr, 0, len(ifaddrs))
	for _, ia := range ifaddrs {
		prefix, err := netip.ParsePrefix(ia.String())
		if err != nil {
			continue
		}
		addrs = append(addrs, prefix.Addr())
	}
	return Usable(addrs), nil
}
