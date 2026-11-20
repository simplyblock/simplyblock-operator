//go:build linux

// Reading a redirected connection's original destination from the kernel's
// connection tracking, which DNAT leaves on the accepted socket.

package hostpath

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

// OriginalDestination returns where conn was headed before the host path's DNAT
// sent it to the forwarder (SO_ORIGINAL_DST).
func OriginalDestination(conn net.Conn) (netip.AddrPort, error) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("%T is not a TCP connection", conn)
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var dst netip.AddrPort
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		// The kernel answers with a struct sockaddr_in. The 16-byte IPv6
		// multicast request is the getsockopt shape golang.org/x/sys offers
		// for a buffer of that size.
		mreq, err := unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
		if err != nil {
			sockErr = err
			return
		}
		b := mreq.Multiaddr
		if family := binary.NativeEndian.Uint16(b[0:2]); family != unix.AF_INET {
			sockErr = fmt.Errorf("original destination of address family %d, not IPv4", family)
			return
		}
		dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), binary.BigEndian.Uint16(b[2:4]))
	}); err != nil {
		return netip.AddrPort{}, err
	}
	if sockErr != nil {
		return netip.AddrPort{}, fmt.Errorf("reading the original destination: %w", sockErr)
	}
	if !dst.IsValid() {
		return netip.AddrPort{}, errors.New("no original destination")
	}
	return dst, nil
}
