// Holding the fleet's workers to one management network and one set of data
// networks.
//
// Each worker's interfaces are chosen from its own report, and nothing in one
// report says what network another worker is on. A storage node reaches its
// peers on both planes, so a cluster whose workers chose links on different
// networks is one whose nodes cannot reach each other, and the control plane
// finds that out after the nodes are added. The pass here compares the choices
// across the fleet and refuses the workers that disagree with the majority.
//
// It compares networks rather than names. Two machines calling their links
// eth0 and ens5f0 are on one network when their addresses share a prefix, and
// two machines both calling theirs eth0 are on two networks when they do not.

package discovery

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// The rules the refusals here name.
const (
	ManagementNetworkRule = "management network"
	DataNetworkRule       = "data network"
)

// maxDataNetworks bounds the networks one worker's data interfaces are compared
// on. The majority is found by counting every subset a worker holds, and a
// machine with more storage networks than this is not one the rule describes.
const maxDataNetworks = 8

// alignNetworks refuses the workers whose management network is not the
// fleet's, then the workers missing one of the fleet's data networks, and
// returns the workers left with their data interfaces narrowed to those
// networks.
func alignNetworks(workers []Worker) ([]Worker, []Refusal) {
	workers, refusals := alignManagement(workers)
	workers, dataRefusals := alignData(workers)
	return workers, append(refusals, dataRefusals...)
}

// alignManagement refuses the workers whose management interface is not on the
// network most of the fleet's management interfaces share.
//
// A worker whose management interface holds no comparable network is not
// compared: a host route names none, and GCP assigns an instance its address as
// one.
func alignManagement(workers []Worker) ([]Worker, []Refusal) {
	networks := make([][]netip.Prefix, len(workers))
	support := map[netip.Prefix]int{}
	for i, worker := range workers {
		networks[i] = networksOf(interfacesByName(worker.Report)[worker.Mgmt.Name])
		for _, network := range networks[i] {
			support[network]++
		}
	}
	if len(support) == 0 {
		return workers, nil
	}

	// The most workers first, and the lower network for a tie, which is
	// determinism rather than a reading: nothing says which of two equal
	// halves of a fleet is the right one.
	fleet := slices.SortedFunc(mapsKeys(support), func(a, b netip.Prefix) int {
		return cmp.Or(cmp.Compare(support[b], support[a]), comparePrefixes(a, b))
	})[0]

	var kept []Worker
	var refusals []Refusal
	for i, worker := range workers {
		if len(networks[i]) == 0 || slices.Contains(networks[i], fleet) {
			kept = append(kept, worker)
			continue
		}
		refusals = append(refusals, Refusal{
			Worker: worker.Name, Rule: ManagementNetworkRule,
			Reason: fmt.Sprintf("%s is on %s, and the fleet's management network is %s, held by %d of its %d workers",
				worker.Mgmt.Name, joinPrefixes(networks[i]), fleet, support[fleet], len(workers)),
		})
	}
	return kept, refusals
}

// alignData narrows every worker's data interfaces to the fleet's data
// networks, and refuses a worker missing one of them.
//
// The fleet's data networks are the set held on data interfaces by more than
// half the workers, the set held by the most of them winning and the larger set
// breaking a tie. Several networks are a multipath data plane, which the
// control plane serves with a listener on each, so a worker holding all but one
// of them is a node its peers reach on fewer paths than the cluster was told it
// has. Where no set reaches a majority there is no data plane to agree on, and
// every worker serves data on its management interface instead.
//
// An interface holding no comparable network, a host route only, is kept as it
// is and counts as neither holding nor missing a network. Unless every worker
// left names a data interface, none does: the control plane binds the data
// interfaces once for the whole cluster, and a worker without them would be
// handed interfaces it does not have.
func alignData(workers []Worker) ([]Worker, []Refusal) {
	held := make([][]netip.Prefix, len(workers))
	for i, worker := range workers {
		held[i] = dataNetworksOf(worker)
	}

	fleet, support := majoritySet(held, len(workers))

	var kept []Worker
	var refusals []Refusal
	for i, worker := range workers {
		if len(held[i]) == 0 && len(worker.Data) > 0 {
			kept = append(kept, worker)
			continue
		}
		missing := slices.DeleteFunc(slices.Clone(fleet), func(network netip.Prefix) bool {
			return slices.Contains(held[i], network)
		})
		if len(missing) > 0 {
			refusals = append(refusals, Refusal{
				Worker: worker.Name, Rule: DataNetworkRule,
				Reason: fmt.Sprintf("it holds no data interface on %s, and the fleet's data networks are %s, held by %d of its %d workers",
					joinPrefixes(missing), joinPrefixes(fleet), support, len(workers)),
			})
			continue
		}
		worker.Data = onNetworks(worker, fleet)
		kept = append(kept, worker)
	}

	if slices.ContainsFunc(kept, func(worker Worker) bool { return len(worker.Data) == 0 }) {
		for i := range kept {
			kept[i].Data = nil
		}
	}
	return kept, refusals
}

// majoritySet is the set of networks more than half of the fleet holds, and how
// many workers hold it, or nothing when no set is held by a majority.
func majoritySet(held [][]netip.Prefix, fleetSize int) ([]netip.Prefix, int) {
	support := map[string]int{}
	sets := map[string][]netip.Prefix{}
	for _, networks := range held {
		for _, subset := range subsetsOf(networks) {
			key := joinPrefixes(subset)
			sets[key] = subset
			support[key]++
		}
	}

	var best string
	for key, count := range support {
		if 2*count <= fleetSize {
			continue
		}
		if best == "" || cmp.Or(
			cmp.Compare(support[best], count),
			cmp.Compare(len(sets[best]), len(sets[key])),
			strings.Compare(key, best),
		) < 0 {
			best = key
		}
	}
	if best == "" {
		return nil, 0
	}
	return sets[best], support[best]
}

// subsetsOf is every nonempty subset of a worker's networks, each in ascending
// order.
func subsetsOf(networks []netip.Prefix) [][]netip.Prefix {
	if len(networks) > maxDataNetworks {
		networks = networks[:maxDataNetworks]
	}
	var out [][]netip.Prefix
	for mask := 1; mask < 1<<len(networks); mask++ {
		var subset []netip.Prefix
		for i, network := range networks {
			if mask&(1<<i) != 0 {
				subset = append(subset, network)
			}
		}
		out = append(out, subset)
	}
	return out
}

// dataNetworksOf is the networks a worker's data interfaces hold, ascending.
func dataNetworksOf(worker Worker) []netip.Prefix {
	index := interfacesByName(worker.Report)
	out := make([]netip.Prefix, 0, len(worker.Data))
	for _, name := range worker.Data {
		out = append(out, dataNetworkOf(index[name])...)
	}
	slices.SortFunc(out, comparePrefixes)
	return slices.Compact(out)
}

// onNetworks is the worker's data interfaces that hold one of the networks
// given or hold no comparable network at all, in the order they were named.
func onNetworks(worker Worker, networks []netip.Prefix) []string {
	index := interfacesByName(worker.Report)
	var out []string
	for _, name := range worker.Data {
		held := dataNetworkOf(index[name])
		if len(held) == 0 || slices.ContainsFunc(held, func(network netip.Prefix) bool {
			return slices.Contains(networks, network)
		}) {
			out = append(out, name)
		}
	}
	return out
}

// networksOf is the networks an interface's addresses are on, ascending and
// without repeats, leaving out the addresses networkOf names none for.
func networksOf(iface nodeprobe.Interface) []netip.Prefix {
	var out []netip.Prefix
	for _, address := range iface.Addresses {
		if network, named := networkOf(address); named {
			out = append(out, network)
		}
	}
	slices.SortFunc(out, comparePrefixes)
	return slices.Compact(out)
}

// dataNetworkOf is the network of a data interface's first IPv4 address, the
// only one the control plane listens on, or none when that address names none.
func dataNetworkOf(iface nodeprobe.Interface) []netip.Prefix {
	first, found := firstIPv4(iface)
	if !found {
		return nil
	}
	if network, named := networkOf(first); named {
		return []netip.Prefix{network}
	}
	return nil
}

// networkOf is the network an address is on. A host route names none, and
// neither does an address nothing could reach the machine on.
func networkOf(address string) (netip.Prefix, bool) {
	prefix, err := netip.ParsePrefix(address)
	if err != nil || !reachable(address) || prefix.IsSingleIP() {
		return netip.Prefix{}, false
	}
	return prefix.Masked(), true
}

// comparePrefixes orders networks by address, then by prefix length.
func comparePrefixes(a, b netip.Prefix) int {
	return cmp.Or(a.Addr().Compare(b.Addr()), cmp.Compare(a.Bits(), b.Bits()))
}

// joinPrefixes renders networks for a reader, comma-separated.
func joinPrefixes(networks []netip.Prefix) string {
	parts := make([]string, 0, len(networks))
	for _, network := range networks {
		parts = append(parts, network.String())
	}
	return strings.Join(parts, ", ")
}

// mapsKeys is a map's keys, as an iterator for the sorted helpers.
func mapsKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}
