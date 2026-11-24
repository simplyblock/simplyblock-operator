// What the runner offers the operator over the link: the export service and the
// role service, both answered by relaying to the guest.
//
// It lives beside the two relays it joins so the capability list and the
// registered services cannot drift apart.

package relay

import (
	"fmt"
	"slices"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

// Services returns the registration of both services and the capabilities to
// advertise.
func Services(tracker *Tracker, mds MDS) (func(grpc.ServiceRegistrar), []string, error) {
	exports, err := nfsexportrpc.NewServer(tracker)
	if err != nil {
		return nil, nil, fmt.Errorf("export relay: %w", err)
	}
	roles, err := nfsexportrpc.NewMDSServer(mds)
	if err != nil {
		return nil, nil, fmt.Errorf("role relay: %w", err)
	}
	register := func(r grpc.ServiceRegistrar) {
		exports.Register(r)
		roles.Register(r)
	}
	return register, slices.Concat(nfsexportrpc.Capabilities(), []string{nfsexportrpc.CapabilityMDS}), nil
}
