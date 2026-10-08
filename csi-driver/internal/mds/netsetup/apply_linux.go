// Applying a Plan to the pod's network namespace: the bridge, the tap, IPv4
// forwarding, and the NAT rules.
//
// Every step is idempotent. The runner's container restarts inside a pod that
// keeps its network namespace, so the bridge, the tap, and the rules of the
// previous run are found and reused rather than created again. The tap is
// persistent and opened by QEMU by name, which is what lets it outlive a QEMU
// process and be handed to the next one.

package netsetup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	tunDevice  = "/dev/net/tun"
	ipForward  = "/proc/sys/net/ipv4/ip_forward"
	tunMajor   = 10
	tunMinor   = 200
	tunDirMode = 0o755
)

// Apply builds the plan's network in the current network namespace and
// installs its rules through run.
func Apply(ctx context.Context, p Plan, run Runner) error {
	// A pod's namespace does not forward by default, and without it the DNAT
	// rule rewrites packets that are then dropped instead of routed.
	if err := os.WriteFile(ipForward, []byte("1"), 0o644); err != nil {
		return fmt.Errorf("enabling IPv4 forwarding: %w", err)
	}

	bridge, err := ensureBridge(p)
	if err != nil {
		return err
	}
	if err := ensureTunDevice(); err != nil {
		return err
	}
	if err := ensureTap(p, bridge); err != nil {
		return err
	}
	return EnsureRules(ctx, run, p.Rules())
}

// DefaultUplink returns the interface carrying the IPv4 default route, which
// in a pod is the pod network's interface. Its name depends on the CNI, so it
// is looked up rather than assumed to be eth0.
func DefaultUplink() (string, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return "", fmt.Errorf("listing routes: %w", err)
	}
	for _, route := range routes {
		if route.Dst != nil && !isDefault(route.Dst) {
			continue
		}
		link, err := netlink.LinkByIndex(route.LinkIndex)
		if err != nil {
			return "", fmt.Errorf("resolving the default route's interface: %w", err)
		}
		return link.Attrs().Name, nil
	}
	return "", errors.New("no IPv4 default route")
}

func isDefault(dst *net.IPNet) bool {
	ones, _ := dst.Mask.Size()
	return ones == 0
}

func ensureBridge(p Plan) (netlink.Link, error) {
	bridge, err := linkByName(p.Bridge)
	if err != nil {
		return nil, err
	}
	if bridge == nil {
		bridge = &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: p.Bridge}}
		if err := netlink.LinkAdd(bridge); err != nil {
			return nil, fmt.Errorf("creating bridge %s: %w", p.Bridge, err)
		}
	}

	gateway := &netlink.Addr{IPNet: &net.IPNet{
		IP:   p.Gateway.Addr().AsSlice(),
		Mask: net.CIDRMask(p.Gateway.Bits(), 32),
	}}
	// AddrReplace rather than AddrAdd: the address is already there after a
	// container restart.
	if err := netlink.AddrReplace(bridge, gateway); err != nil {
		return nil, fmt.Errorf("addressing bridge %s: %w", p.Bridge, err)
	}
	if err := netlink.LinkSetUp(bridge); err != nil {
		return nil, fmt.Errorf("bringing bridge %s up: %w", p.Bridge, err)
	}
	return bridge, nil
}

// ensureTunDevice creates /dev/net/tun when the container's /dev lacks it. A
// privileged container normally sees the host's, so this is the fallback for a
// runtime that hands it a minimal /dev.
func ensureTunDevice() error {
	if _, err := os.Stat(tunDevice); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking %s: %w", tunDevice, err)
	}
	if err := os.MkdirAll("/dev/net", tunDirMode); err != nil {
		return fmt.Errorf("creating /dev/net: %w", err)
	}
	dev := int(unix.Mkdev(tunMajor, tunMinor))
	if err := unix.Mknod(tunDevice, unix.S_IFCHR|0o666, dev); err != nil {
		return fmt.Errorf("creating %s: %w", tunDevice, err)
	}
	return nil
}

func ensureTap(p Plan, bridge netlink.Link) error {
	tap, err := linkByName(p.Tap)
	if err != nil {
		return err
	}
	if tap == nil {
		// Single-queue with a virtio-net header, matching how QEMU opens it
		// (-netdev tap without queues=). A multi-queue tap would refuse a
		// single-queue open.
		tap = &netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: p.Tap},
			Mode:      netlink.TUNTAP_MODE_TAP,
			Flags:     netlink.TUNTAP_DEFAULTS,
		}
		if err := netlink.LinkAdd(tap); err != nil {
			return fmt.Errorf("creating tap %s: %w", p.Tap, err)
		}
	}
	if err := netlink.LinkSetMaster(tap, bridge); err != nil {
		return fmt.Errorf("attaching tap %s to bridge %s: %w", p.Tap, p.Bridge, err)
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		return fmt.Errorf("bringing tap %s up: %w", p.Tap, err)
	}
	return nil
}

// linkByName returns the named link, or nil when it does not exist.
func linkByName(name string) (netlink.Link, error) {
	link, err := netlink.LinkByName(name)
	if err == nil {
		return link, nil
	}
	var notFound netlink.LinkNotFoundError
	if errors.As(err, &notFound) {
		return nil, nil
	}
	return nil, fmt.Errorf("looking up %s: %w", name, err)
}
