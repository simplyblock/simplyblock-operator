// The verification of an attach: Multus's network-status annotation on the SPDK
// pod compared with the declaration. It is written once, when the attach finishes.
//
// Specified by operator/docs/designs/design-storage-network.md §5.4.

package node

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// NetworkStatusAnnotation is Multus's record of the networks a pod attached
	// to, with the interface and the addresses each holds.
	NetworkStatusAnnotation = "k8s.v1.cni.cncf.io/network-status"

	// spdkPodAppLabel and spdkPodAppPrefix name the label that carries a storage
	// node's RPC port on its SPDK pod: app=spdk-app-<port>. The control plane
	// writes it, and the port is what tells the several SPDK pods of one worker
	// apart. It does not tell the workers apart: every worker's first SPDK pod
	// carries the same port, so a lookup pairs it with the worker.
	spdkPodAppLabel  = "app"
	spdkPodAppPrefix = "spdk-app-"
)

// NetworkVerdict is the comparison of one declared attachment with what the pod
// reports for it.
type NetworkVerdict struct {
	Network   string
	Interface string

	// Declared is the address the node states, with its prefix length, and
	// Reported the address the pod holds on the interface. Reported is empty when
	// the pod holds no such interface.
	Declared string
	Reported string

	Phase simplyblockv1alpha2.NodeNetworkPhase
}

// podNetwork is one element of the network-status annotation.
type podNetwork struct {
	Name      string   `json:"name"`
	Interface string   `json:"interface"`
	IPs       []string `json:"ips"`
}

// VerifyAttachments compares each declared attachment with the pod's
// network-status, matched by definition namespace, name, and interface. The
// default network and undeclared interfaces are ignored. A missing interface is
// Pending until the pod runs and Unavailable after. A different address is
// Mismatched, compared by host part.
func VerifyAttachments(declared []Attachment, annotation string, podRunning bool) ([]NetworkVerdict, error) {
	var reported []podNetwork
	if annotation != "" {
		if err := json.Unmarshal([]byte(annotation), &reported); err != nil {
			return nil, fmt.Errorf("reading the %s annotation: %w", NetworkStatusAnnotation, err)
		}
	}

	verdicts := make([]NetworkVerdict, 0, len(declared))
	for _, want := range declared {
		verdict := NetworkVerdict{Network: want.Network, Interface: want.Interface}
		if len(want.IPs) > 0 {
			verdict.Declared = want.IPs[0]
		}

		held, found := heldInterface(reported, want)
		switch {
		case !found && podRunning:
			verdict.Phase = simplyblockv1alpha2.NodeNetworkPhaseUnavailable
		case !found:
			verdict.Phase = simplyblockv1alpha2.NodeNetworkPhasePending
		default:
			verdict.Reported, verdict.Phase = compareAddress(want, held)
		}
		verdicts = append(verdicts, verdict)
	}
	return verdicts, nil
}

// heldInterface finds the annotation's element for a declared attachment.
func heldInterface(reported []podNetwork, want Attachment) (podNetwork, bool) {
	name := want.Name
	if want.Namespace != "" {
		name = want.Namespace + "/" + want.Name
	}
	for _, r := range reported {
		if r.Name == name && r.Interface == want.Interface {
			return r, true
		}
	}
	return podNetwork{}, false
}

// compareAddress returns the address the pod holds on a matched interface and
// whether it is the declared one.
func compareAddress(want Attachment, held podNetwork) (string, simplyblockv1alpha2.NodeNetworkPhase) {
	if len(want.IPs) == 0 {
		return firstOr(held.IPs, ""), simplyblockv1alpha2.NodeNetworkPhaseAttached
	}
	wantHost, ok := hostOf(want.IPs[0])
	for _, ip := range held.IPs {
		if got, gotOK := hostOf(ip); ok && gotOK && got == wantHost {
			return ip, simplyblockv1alpha2.NodeNetworkPhaseAttached
		}
	}
	return firstOr(held.IPs, ""), simplyblockv1alpha2.NodeNetworkPhaseMismatched
}

// hostOf is the address part of an address written with or without a prefix
// length.
func hostOf(address string) (netip.Addr, bool) {
	if prefix, err := netip.ParsePrefix(address); err == nil {
		return prefix.Addr(), true
	}
	addr, err := netip.ParseAddr(address)
	return addr, err == nil
}

func firstOr(values []string, fallback string) string {
	if len(values) > 0 {
		return values[0]
	}
	return fallback
}

// SPDKPodFor picks a storage node's SPDK pod by its worker and its RPC port. A
// worker runs several SPDK pods behind one agent, and the port tells those
// apart; every worker runs a pod on the same first port, and the worker tells
// those apart. Matching the port alone read worker-1's pod for worker-2 and
// worker-3 of a supercluster and judged their addresses mismatched (Talos lab,
// 2026-10-25). An empty workerNode matches any worker, for a list a caller has
// already narrowed to one.
func SPDKPodFor(pods []corev1.Pod, workerNode string, rpcPort int32) (*corev1.Pod, bool) {
	want := spdkPodAppPrefix + strconv.Itoa(int(rpcPort))
	for i := range pods {
		if pods[i].Labels[spdkPodAppLabel] != want {
			continue
		}
		if workerNode != "" && pods[i].Spec.NodeName != workerNode {
			continue
		}
		return &pods[i], true
	}
	return nil, false
}
