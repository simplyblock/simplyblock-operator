// The address of a node on a network attached through a definition is the
// address of the NIC the definition moves, read before the attach. These tests
// cover the choice among a NIC's addresses, the definitions a storage node can
// use, and the holds that name a missing address, a missing interface, or a
// worker with no host check.

package node

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/inventory"

	"github.com/simplyblock/simplyblock-operator/internal/multus"
)

const (
	// storageNetwork is the network the tests declare, and adoptedAddress the
	// address its NIC holds.
	storageNetwork = "storage-a"
	adoptedAddress = "10.10.10.147/24"

	hostDeviceStatic = `{"cniVersion":"0.3.1","type":"host-device","device":"ens4","capabilities":{"ips":true},"ipam":{"type":"static"}}`
	nicName          = "ens4"
	nicSlot          = "0000:00:06.0"
)

// fakeHost is the CSI node plugins as the operator sees them: the interfaces of
// the worker, and whether a plugin can answer at all.
type fakeHost struct {
	interfaces  map[string]InterfaceReport
	unavailable bool
	reads       []InterfaceSelector

	// workers are the workers with a plugin linked, which is the node's own
	// worker when unset. noWorkers says none is linked.
	workers   []string
	noWorkers bool
	// refused are the workers whose connection fails, with the reason.
	refused map[string]string
	// connectErr fails the call itself, which is not an answer.
	connectErr error
	// connects records each connection asked for, as worker, then address:port.
	connects []string
}

func (f *fakeHost) InterfaceAddresses(_ context.Context, _ string, s InterfaceSelector) (InterfaceReport, error) {
	f.reads = append(f.reads, s)
	if f.unavailable {
		return InterfaceReport{}, ErrHostCheckUnavailable
	}
	return f.interfaces[s.Name+s.PCIAddress], nil
}

func (f *fakeHost) Connect(_ context.Context, worker, address string, port int) (bool, string, error) {
	f.connects = append(f.connects, fmt.Sprintf("%s %s:%d", worker, address, port))
	if f.connectErr != nil {
		return false, "", f.connectErr
	}
	if detail, refused := f.refused[worker]; refused {
		return false, detail, nil
	}
	return true, "", nil
}

func (f *fakeHost) Workers() []string {
	switch {
	case f.noWorkers:
		return nil
	case len(f.workers) > 0:
		return f.workers
	}
	return []string{opsWorker}
}

// aWorkerWithTheNIC is a worker whose NIC holds the given addresses.
func aWorkerWithTheNIC(addresses ...string) *fakeHost {
	return &fakeHost{interfaces: map[string]InterfaceReport{nicName: {Found: true, Addresses: addresses}}}
}

func aDefinitionConfigured(config string) *unstructured.Unstructured {
	nad := multus.NewDefinition()
	nad.SetNamespace(opsNamespace)
	nad.SetName("storage-a")
	if err := unstructured.SetNestedField(nad.Object, config, "spec", "config"); err != nil {
		panic(err)
	}
	return nad
}

// adoptWorld is a node on one network whose definition carries the given
// configuration, with nothing recorded yet and the host check in place.
func adoptWorld(t *testing.T, config string, host HostChecker) (*StorageNodeReconciler, client.Client) {
	t.Helper()
	r, apiClient := onANetwork(t, onDefinition(storageNetwork), aDefinitionConfigured(config))
	node := nodeRead(t, apiClient)
	node.Status.Network = nil
	if err := apiClient.Status().Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	r.HostCheck = host
	return r, apiClient
}

func runCheck(t *testing.T, r *StorageNodeReconciler) (nodeStep, bool, error) {
	t.Helper()
	node, cluster := nodeAndClusterOf(t, r)
	return r.checkNetwork(context.Background(), node, cluster)
}

func holdReason(t *testing.T, err error) string {
	t.Helper()
	var blocked *blockedStepError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want the node held with a reason", err)
	}
	return blocked.reason
}

func assignedOn(t *testing.T, apiClient client.Client) string {
	t.Helper()
	for _, n := range nodeRead(t, apiClient).Status.Network {
		if n.Network == storageNetwork {
			return n.Assigned
		}
	}
	return ""
}

// U-44
func TestTheNodesAddressIsTheFirstIPv4AddressOfTheNICWithItsPrefix(t *testing.T) {
	got, ok := usableAddress([]string{"fe80::1/64", adoptedAddress, "10.10.20.1/24"})
	if !ok || got != adoptedAddress {
		t.Errorf("usableAddress = %q, %v, want 10.10.10.147/24", got, ok)
	}
}

// U-45
func TestANICWithoutAUsableAddressHasNone(t *testing.T) {
	cases := map[string][]string{
		"no address":         nil,
		"only IPv6":          {"fd00::5/64", "fe80::1/64"},
		"only link-local":    {"169.254.3.4/16"},
		"only loopback":      {"127.0.0.1/8"},
		"an unparsable text": {"not an address"},
	}
	for name, addresses := range cases {
		if got, ok := usableAddress(addresses); ok {
			t.Errorf("%s: usableAddress = %q, want none", name, got)
		}
	}
}

// U-46
func TestTheSelectorNamesTheDeviceOrThePCISlot(t *testing.T) {
	byName := selectorOf(inventory.ParseNetworkAttachmentConfig(hostDeviceStatic))
	if byName != (InterfaceSelector{Name: nicName}) {
		t.Errorf("selector = %+v, want the interface name", byName)
	}
	bySlot := selectorOf(inventory.ParseNetworkAttachmentConfig(
		`{"type":"host-device","pciBusID":"` + nicSlot + `","ipam":{"type":"static"}}`))
	if bySlot != (InterfaceSelector{PCIAddress: nicSlot}) {
		t.Errorf("selector = %+v, want the PCI address", bySlot)
	}
}

// I-57
func TestAHostDeviceNetworkRecordsTheNICAddressBeforeTheNodeAdd(t *testing.T) {
	r, apiClient := adoptWorld(t, hostDeviceStatic, aWorkerWithTheNIC("fe80::1/64", adoptedAddress))

	step, done, err := runCheck(t, r)
	if err != nil || !done || step != stepAwaitingSlot {
		t.Fatalf("check = %s, %v, %v, want the node to move on to the queue", step, done, err)
	}
	if got := assignedOn(t, apiClient); got != adoptedAddress {
		t.Errorf("assigned = %q, want the NIC's address with its prefix", got)
	}
}

// I-58
func TestANICWithoutAnAddressHoldsTheNodeAndRecordsNothing(t *testing.T) {
	r, apiClient := adoptWorld(t, hostDeviceStatic, aWorkerWithTheNIC("fe80::1/64"))

	_, done, err := runCheck(t, r)
	if done || holdReason(t, err) != NetworkInterfaceNoAddress {
		t.Fatalf("done=%v err=%v, want the node held for %s", done, err, NetworkInterfaceNoAddress)
	}
	if got := assignedOn(t, apiClient); got != "" {
		t.Errorf("assigned = %q, want nothing recorded", got)
	}
}

// I-71, I-72
func TestAWorkerWithoutAHostCheckHoldsTheNode(t *testing.T) {
	cases := map[string]HostChecker{
		"no host check configured": nil,
		"no plugin linked":         &fakeHost{unavailable: true},
	}
	for name, host := range cases {
		r, apiClient := adoptWorld(t, hostDeviceStatic, host)
		_, done, err := runCheck(t, r)
		if done || holdReason(t, err) != NetworkHostCheckUnavailable {
			t.Errorf("%s: done=%v err=%v, want the node held for %s", name, done, err, NetworkHostCheckUnavailable)
		}
		if got := assignedOn(t, apiClient); got != "" {
			t.Errorf("%s: assigned = %q, want nothing recorded", name, got)
		}
	}
}

// An interface the worker lacks is a failed attachment that names it.
func TestAnInterfaceTheWorkerLacksHoldsTheNode(t *testing.T) {
	r, _ := adoptWorld(t, hostDeviceStatic, &fakeHost{interfaces: map[string]InterfaceReport{}})

	_, done, err := runCheck(t, r)
	if done || holdReason(t, err) != NetworkAttachmentFailed {
		t.Fatalf("done=%v err=%v, want the node held for %s", done, err, NetworkAttachmentFailed)
	}
}

// I-74
func TestARecordedAddressIsNeverReadAgain(t *testing.T) {
	host := aWorkerWithTheNIC(adoptedAddress)
	r, apiClient := adoptWorld(t, hostDeviceStatic, host)
	if _, done, err := runCheck(t, r); err != nil || !done {
		t.Fatalf("first check: done=%v err=%v", done, err)
	}
	reads := len(host.reads)

	// The NIC is in the SPDK pod now and the host shows no such interface.
	host.interfaces = map[string]InterfaceReport{}
	if _, done, err := runCheck(t, r); err != nil || !done {
		t.Fatalf("second check: done=%v err=%v, want the recorded address kept", done, err)
	}
	if len(host.reads) != reads {
		t.Errorf("the worker was read %d more times, want none once the address is recorded", len(host.reads)-reads)
	}
	if got := assignedOn(t, apiClient); got != adoptedAddress {
		t.Errorf("assigned = %q, want the first reading kept", got)
	}
}
