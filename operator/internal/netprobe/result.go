// What the probe pods report, read from their termination messages.

package netprobe

import (
	"encoding/json"
	"fmt"
)

// GuestResult is what the guest probe found from inside the attached pod.
type GuestResult struct {
	Interfaces map[string]InterfaceResult
	duplicates map[string]int
	answering  map[string][]string
	peers      map[string]map[string]bool
}

// InterfaceResult is what the probe read of one interface.
type InterfaceResult struct {
	MTU int
}

// AddressInUse reports whether another host answered the duplicate-address check
// for the address declared on the interface.
func (r GuestResult) AddressInUse(iface string) bool {
	return r.duplicates[iface] > 0
}

// AnsweringAddresses are the addresses another host answered for on the
// interface, in the order the probe checked them: the address the probe holds and
// the candidates it only asked about.
func (r GuestResult) AnsweringAddresses(iface string) []string {
	return r.answering[iface]
}

// PeerReached reports whether the peer answered a ping sent out of the
// interface. A peer the probe never checked was not reached.
func (r GuestResult) PeerReached(iface, peer string) bool {
	return r.peers[iface][peer]
}

// ParseGuest reads the guest probe's termination message.
func ParseGuest(message string) (GuestResult, error) {
	var raw struct {
		Interfaces []struct {
			Name string `json:"name"`
			MTU  int    `json:"mtu"`
		} `json:"interfaces"`
		Duplicates []struct {
			Interface string `json:"interface"`
			Address   string `json:"address"`
			Replies   int    `json:"replies"`
		} `json:"duplicates"`
		Peers []struct {
			Interface string `json:"interface"`
			Peer      string `json:"peer"`
			OK        bool   `json:"ok"`
		} `json:"peers"`
	}
	if err := json.Unmarshal([]byte(message), &raw); err != nil {
		return GuestResult{}, fmt.Errorf("reading the guest probe's result: %w", err)
	}

	result := GuestResult{
		Interfaces: map[string]InterfaceResult{}, duplicates: map[string]int{},
		answering: map[string][]string{}, peers: map[string]map[string]bool{},
	}
	for _, i := range raw.Interfaces {
		result.Interfaces[i.Name] = InterfaceResult{MTU: i.MTU}
	}
	for _, d := range raw.Duplicates {
		result.duplicates[d.Interface] += d.Replies
		if d.Replies > 0 {
			result.answering[d.Interface] = append(result.answering[d.Interface], d.Address)
		}
	}
	for _, p := range raw.Peers {
		if result.peers[p.Interface] == nil {
			result.peers[p.Interface] = map[string]bool{}
		}
		result.peers[p.Interface][p.Peer] = p.OK
	}
	return result, nil
}

// HostResult is what the host probe found.
type HostResult struct {
	targets map[string]bool
}

// Reached reports whether the target answered. A target the probe never checked
// was not reached.
func (r HostResult) Reached(address string) bool {
	return r.targets[address]
}

// AllReached reports whether every target answered, and is false for a result
// that checked none.
func (r HostResult) AllReached() bool {
	if len(r.targets) == 0 {
		return false
	}
	for _, ok := range r.targets {
		if !ok {
			return false
		}
	}
	return true
}

// ParseHost reads the host probe's termination message.
func ParseHost(message string) (HostResult, error) {
	var raw struct {
		Targets []struct {
			Address string `json:"address"`
			OK      bool   `json:"ok"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(message), &raw); err != nil {
		return HostResult{}, fmt.Errorf("reading the host probe's result: %w", err)
	}
	result := HostResult{targets: map[string]bool{}}
	for _, t := range raw.Targets {
		result.targets[t.Address] = t.OK
	}
	return result, nil
}
