// The runner's two names, which differ since the metadata server has standbys:
// the guest takes the StatefulSet's name so every guest presents one NFS server
// identity, and the link peer takes the pod's name so each pod is addressable.

package runner

import (
	"errors"

	"github.com/simplyblock/atlas/link"
)

// PeerID is the peer the pod registers as: its own pod name, never the
// guest's hostname.
func PeerID(podName, hostname string) (link.PeerID, error) {
	if podName == "" {
		return link.PeerID{}, errors.New("no pod name: the pod cannot register as a peer")
	}
	if hostname == "" {
		return link.PeerID{}, errors.New("no guest hostname: it is the StatefulSet's name")
	}
	return link.MDSPeer(podName), nil
}
