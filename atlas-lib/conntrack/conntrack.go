// Package conntrack forgets a host's connection-tracking entries for one dead
// backend: the flows of one protocol and destination port that the kernel
// translated to one address.
//
// A Service's ClusterIP is translated to a backend when a connection's first
// packet passes kube-proxy's rules, and the translation is kept in conntrack for
// the life of the entry. When the backend goes away, an entry already pointing
// at it keeps pointing at it, and a client that retries its SYN on the same
// tuple keeps the entry alive: run pnfs-1791575321 saw pNFS clients stay
// translated to a replaced metadata server pod for up to two minutes. kube-proxy
// clears such entries for UDP only. Forgetting them is node-level work, done by
// the CSI node plugin on the host's network, and asked for by the operator, so
// it lives here rather than in either consumer.
//
// A Selector names one protocol, one original destination port, and one reply
// source, the address a flow was translated to, and nothing broader. Flushing a
// node's table would break every other workload's connections.
package conntrack

import (
	"errors"
	"fmt"
	"net/netip"
)

// ErrInvalidSelector is a selector that does not name exactly one protocol,
// port, and address.
var ErrInvalidSelector = errors.New("conntrack: invalid selector")

// Protocol is an IP protocol number.
type Protocol uint8

// The protocols a Selector can name.
const (
	TCP Protocol = 6
	UDP Protocol = 17
)

// Selector names the flows to forget.
type Selector struct {
	// Protocol of the flows.
	Protocol Protocol
	// DstPort is the destination port the client connected to, before any
	// translation: a Service's port.
	DstPort uint16
	// ReplySource is the address replies come from, which for a translated flow
	// is the backend the kernel chose: the address being forgotten.
	ReplySource netip.Addr
}

// Validate refuses a selector that would match more than one backend's flows.
func (s Selector) Validate() error {
	switch {
	case s.Protocol != TCP && s.Protocol != UDP:
		return fmt.Errorf("%w: protocol %d", ErrInvalidSelector, s.Protocol)
	case s.DstPort == 0:
		return fmt.Errorf("%w: no destination port", ErrInvalidSelector)
	case !s.ReplySource.IsValid() || s.ReplySource.IsUnspecified():
		return fmt.Errorf("%w: no reply source address", ErrInvalidSelector)
	}
	return nil
}

// Tuple is what a selector compares of one conntrack entry.
type Tuple struct {
	Protocol Protocol
	// OrigDst and OrigDstPort are where the client connected to.
	OrigDst     netip.Addr
	OrigDstPort uint16
	// ReplySource is where the replies come from: the translated backend.
	ReplySource netip.Addr
}

// Matches reports whether the entry is one of the selected flows.
func (s Selector) Matches(t Tuple) bool {
	return t.Protocol == s.Protocol && t.OrigDstPort == s.DstPort && t.ReplySource == s.ReplySource
}

// Forget deletes the host's entries the selector matches and returns how many
// it deleted. It needs CAP_NET_ADMIN in the host's network namespace, and off
// Linux it returns errs.ErrUnsupported.
func Forget(s Selector) (uint, error) {
	if err := s.Validate(); err != nil {
		return 0, err
	}
	return forget(s)
}
