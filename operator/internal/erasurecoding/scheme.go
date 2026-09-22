// The erasure-coding schemes simplyblock supports, and how many storage nodes
// each one needs before a cluster may be built with it.
//
// Both facts are mirrored from elsewhere, and neither is enforced where it is
// first written down. The scheme set is simplyblock_core's
// SUPPORTED_ERASURE_CODING_SCHEMES, which the control plane checks on the
// create call — by which point the operator has already written a
// StorageCluster and an administrator has already approved the document that
// produced it. The minimum-node table is the product documentation's, and the
// control plane checks nothing resembling it: its activation gate counts
// devices (ndcs+npcs+1) and never nodes, so a cluster with too few nodes for
// its stripe activates and loses data on the first failure it was configured to
// survive.
//
// The rules live in a package of their own because three unrelated callers ask
// them: the deployment config's validation and its approval webhook, the
// cluster operation's activation gate, and the discovery run that proposes a
// scheme for a fleet it has just inspected. A leaf package with no Kubernetes
// dependencies beyond the API types is what lets all three import it.

package erasurecoding

import (
	"fmt"
	"strings"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Scheme is one erasure-coding layout: the data chunks a stripe carries and the
// parity chunks protecting them, which the documentation writes as k+m and the
// control plane as distr_ndcs and distr_npcs.
type Scheme struct {
	// DataChunks is ndcs, the k of the notation.
	DataChunks int

	// ParityChunks is npcs, the m of the notation, and therefore how many
	// simultaneous failures the cluster survives.
	ParityChunks int
}

// supported is the control plane's set, in the order the documentation lists
// it: the failure tolerances in turn, and the data widths ascending within
// each. The order is the one a reviewer reading a refusal sees, so it is the
// documentation's rather than sorted.
var supported = []Scheme{
	{DataChunks: 1, ParityChunks: 0},
	{DataChunks: 1, ParityChunks: 1},
	{DataChunks: 2, ParityChunks: 1},
	{DataChunks: 4, ParityChunks: 1},
	{DataChunks: 1, ParityChunks: 2},
	{DataChunks: 2, ParityChunks: 2},
	{DataChunks: 4, ParityChunks: 2},
}

// Supported is every scheme a cluster may be built with.
func Supported() []Scheme {
	out := make([]Scheme, len(supported))
	copy(out, supported)
	return out
}

// IsSupported reports whether the control plane would accept this scheme.
func (s Scheme) IsSupported() bool {
	for _, candidate := range supported {
		if candidate == s {
			return true
		}
	}
	return false
}

// MinimumNodes is how many storage nodes a cluster needs before it may be built
// with this scheme.
//
// It is ndcs+npcs nodes to place a stripe across, plus one spare node per
// tolerated failure for the rebuild to land on: a cluster with exactly
// ndcs+npcs nodes survives a failure with no node left to reconstruct the lost
// chunks onto, so it is one failure from unprotected and a second from data
// loss. That reproduces the documented table exactly, including the 1+0 that
// protects nothing and therefore needs no spare.
func (s Scheme) MinimumNodes() int {
	return s.DataChunks + 2*s.ParityChunks
}

// String is the k+m notation the documentation, the control plane's Mod column,
// and StorageCluster.status.erasureCodingScheme all use.
func (s Scheme) String() string {
	return fmt.Sprintf("%d+%d", s.DataChunks, s.ParityChunks)
}

// SchemeOf reads a cluster's stripe, filling in what an unstated one means.
//
// An absent stripe, or one stating only half of itself, is 1+1: that is what
// the control plane's API defaults each field to, and what the operator sends
// for a cluster whose spec says nothing. A document that never mentions erasure
// coding therefore describes a cluster needing three nodes, which is the whole
// reason the default has to be resolved here rather than left unknown.
func SchemeOf(stripe *simplyblockv1alpha2.StripeSpec) Scheme {
	if stripe == nil {
		return Scheme{DataChunks: 1, ParityChunks: 1}
	}
	return Scheme{
		DataChunks:   ptr.IntFrom(stripe.DataChunks, 1),
		ParityChunks: ptr.IntFrom(stripe.ParityChunks, 1),
	}
}

// SupportedNotation renders the supported set as one phrase for a message,
// because a refusal that names the scheme it rejected without naming the
// alternatives leaves the reviewer to go and find them.
func SupportedNotation() string {
	rendered := make([]string, 0, len(supported))
	for _, scheme := range supported {
		rendered = append(rendered, scheme.String())
	}
	last := len(rendered) - 1
	return strings.Join(rendered[:last], ", ") + ", and " + rendered[last]
}
