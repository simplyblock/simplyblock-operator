// Tests for the host-side checks: the choice of interface by name or by PCI
// slot, the refusal of a selector that names nothing, and a TCP connection that
// succeeds, is refused, or is cut off by its context.

package netcheck

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/simplyblock/atlas/inventory"
)

var hostInterfaces = []inventory.Interface{
	{Name: "ens5", PCIAddress: "0000:00:05.0", Addresses: []string{"10.10.0.5/20"}},
	{Name: "ens6", PCIAddress: "0000:00:06.0", Addresses: []string{"fe80::1/64", "10.10.100.8/24"}},
	{Name: "lo", Addresses: []string{"127.0.0.1/8"}},
}

func TestAnInterfaceIsFoundByNameAndByPCIAddress(t *testing.T) {
	byName := Lookup(hostInterfaces, Selector{Name: "ens6"})
	if !byName.Found || len(byName.Addresses) != 2 || byName.Addresses[1] != "10.10.100.8/24" {
		t.Errorf("by name = %+v, want ens6 with its two addresses in host order", byName)
	}
	bySlot := Lookup(hostInterfaces, Selector{PCIAddress: "0000:00:05.0"})
	if !bySlot.Found || len(bySlot.Addresses) != 1 || bySlot.Addresses[0] != "10.10.0.5/20" {
		t.Errorf("by slot = %+v, want ens5", bySlot)
	}
}

func TestAnInterfaceTheHostLacksIsNotFound(t *testing.T) {
	for _, sel := range []Selector{{Name: "ens7"}, {PCIAddress: "0000:00:09.0"}} {
		if got := Lookup(hostInterfaces, sel); got.Found || len(got.Addresses) != 0 {
			t.Errorf("%+v: got %+v, want not found and no addresses", sel, got)
		}
	}
}

func TestASelectorThatNamesNothingIsInvalid(t *testing.T) {
	if err := (Selector{}).Validate(); !errors.Is(err, ErrInvalidSelector) {
		t.Errorf("Validate() = %v, want ErrInvalidSelector", err)
	}
	for _, sel := range []Selector{{Name: "ens6"}, {PCIAddress: "0000:00:06.0"}} {
		if err := sel.Validate(); err != nil {
			t.Errorf("%+v: Validate() = %v, want none", sel, err)
		}
	}
}

func listener(t *testing.T) (string, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	addr := l.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func TestAConnectionToAListenerSucceeds(t *testing.T) {
	address, port := listener(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Connect(ctx, address, port); err != nil {
		t.Errorf("Connect() = %v, want it to connect", err)
	}
}

func TestAConnectionToAClosedPortFails(t *testing.T) {
	address, port := listener(t)
	// Take the listener's port and close it, so nothing answers there.
	l, err := net.Listen("tcp", address+":0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	_ = port

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Connect(ctx, address, closedPort); err == nil {
		t.Error("Connect() succeeded against a closed port")
	}
}

func TestAConnectionThatIsCutOffByItsContextFails(t *testing.T) {
	address, port := listener(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Connect(ctx, address, port); err == nil {
		t.Error("Connect() succeeded with a cancelled context")
	}
}
