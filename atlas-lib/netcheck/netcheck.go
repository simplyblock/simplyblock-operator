// Package netcheck answers two questions about a host's network from its own
// namespace: what addresses an interface holds, and whether a TCP connection
// to an address and port succeeds. The operator asks them of the CSI node
// plugin before a node's NIC moves into a pod and after the pod listens (see
// netcheckrpc).
package netcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"

	"github.com/simplyblock/atlas/inventory"
)

// ErrInvalidSelector says a selector names no interface.
var ErrInvalidSelector = errors.New("netcheck: invalid selector")

// Selector names a host interface by its name or by the PCI address of its
// device. The name wins when both are set.
type Selector struct {
	Name       string
	PCIAddress string
}

// String names the interface for a message.
func (s Selector) String() string {
	if s.Name != "" {
		return s.Name
	}
	return s.PCIAddress
}

// Validate refuses a selector that names nothing.
func (s Selector) Validate() error {
	if s.Name == "" && s.PCIAddress == "" {
		return fmt.Errorf("%w: it names neither an interface nor a PCI address", ErrInvalidSelector)
	}
	return nil
}

// Report is what the host shows for one interface.
type Report struct {
	// Found is whether the host has the interface.
	Found bool
	// Addresses are its addresses in CIDR form, in the order the host reports
	// them.
	Addresses []string
}

// Lookup finds the interface a selector names among interfaces.
func Lookup(interfaces []inventory.Interface, sel Selector) Report {
	for _, iface := range interfaces {
		matches := iface.Name == sel.Name
		if sel.Name == "" {
			matches = sel.PCIAddress != "" && iface.PCIAddress == sel.PCIAddress
		}
		if matches {
			return Report{Found: true, Addresses: slices.Clone(iface.Addresses)}
		}
	}
	return Report{}
}

// Interface reads the host's interfaces and looks the selector up.
func Interface(cfg inventory.Config, sel Selector) (Report, error) {
	if err := sel.Validate(); err != nil {
		return Report{}, err
	}
	interfaces, err := inventory.ReadInterfaces(cfg)
	if err != nil {
		return Report{}, fmt.Errorf("netcheck: read the host's interfaces: %w", err)
	}
	return Lookup(interfaces, sel), nil
}

// Connect opens a TCP connection to the address and port and closes it. Nil
// means it connected.
func Connect(ctx context.Context, address string, port int) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}
