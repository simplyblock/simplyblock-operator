// The control plane's answer in the shape a volume stack takes it.
//
// It lives here because the controller-loss timeout below is this package's
// policy, and because both callers are above it: the node plugin's stage path
// and the metadata server's export assembly. One translation, so the two cannot
// disagree about how a namespace is connected or under which identity.

package initiator

import (
	"strings"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/ptr"

	"github.com/simplyblock/csi-driver/internal/controlplane"
)

// ConnectionFrom is where a volume is published, together with the host
// identity the connect has to present.
//
// The endpoints keep the order they were returned in, which is the control
// plane's priority order, so the primary path is attached first.
//
// The controller-loss timeout is this node's rather than the control plane's:
// it is a local policy about how long the kernel keeps retrying a path before
// failing I/O on it.
func ConnectionFrom(
	responses []*controlplane.LvolConnectResp,
	namespaceUUID string,
) (connection lvol.Connection, hostNQN string) {
	if len(responses) == 0 {
		return lvol.Connection{}, ""
	}

	connection = lvol.Connection{
		NQN:  responses[0].Nqn,
		NSID: uint32(responses[0].NSID), //nolint:gosec // a namespace id the control plane reports
		UUID: namespaceUUID,
	}
	for _, response := range responses {
		secret, ctrlSecret, tls, host := connectAuth(response.Connect)
		if host != "" {
			hostNQN = host
		}
		connection.Endpoints = append(connection.Endpoints, lvol.Endpoint{
			Transport:         strings.ToLower(response.TargetType),
			Address:           response.IP,
			Port:              response.Port,
			NrIOQueues:        response.NrIoQueues,
			ReconnectDelaySec: response.ReconnectDelay,
			CtrlLossTMOSec:    ptr.To(DefaultCtrlLossTmo),
			HostIface:         response.HostIface,
			TLS:               tls,
			DHCHAPSecret:      secret,
			DHCHAPCtrlSecret:  ctrlSecret,
		})
	}
	return connection, hostNQN
}

// connectAuth reads the host identity and the DHCHAP key material out of the
// connect command line the control plane built for this host.
//
// That line is the only channel the driver has for them: the control plane is
// the only party that resolves a host's secret, whether pool-shared or
// per-host, and it bakes the flags into the connect string rather than exposing
// them as fields of their own.
func connectAuth(connect string) (secret, ctrlSecret string, tls bool, hostNQN string) {
	for _, field := range strings.Fields(connect) {
		switch {
		case strings.HasPrefix(field, "--hostnqn="):
			hostNQN = strings.TrimPrefix(field, "--hostnqn=")
		case strings.HasPrefix(field, "--dhchap-secret="):
			secret = strings.TrimPrefix(field, "--dhchap-secret=")
		case strings.HasPrefix(field, "--dhchap-ctrl-secret="):
			ctrlSecret = strings.TrimPrefix(field, "--dhchap-ctrl-secret=")
		case field == "--tls":
			tls = true
		}
	}
	return secret, ctrlSecret, tls, hostNQN
}
