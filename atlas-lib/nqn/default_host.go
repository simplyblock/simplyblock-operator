// The node's default host NQN, which nvme-cli presents on a connect that names
// none. It lives beside Host because both are host identities of the same node
// that the control plane has to know about: an access-controlled volume
// connects as Host(nodeUID), a plain one as this default, and a two-node fence
// revokes both.

package nqn

import (
	"fmt"
	"strings"
)

// NVMeHostPrefix is the naming-authority prefix of the NVMe-spec UUID-based
// host NQN, the form nvme-cli generates and the CSI node plugin's postStart
// hook writes from the persisted hostid.
const NVMeHostPrefix = "nqn.2014-08.org.nvmexpress"

const (
	// DefaultHostNQNFile is where nvme-cli reads the host NQN of a connect
	// that names none.
	DefaultHostNQNFile = "/etc/nvme/hostnqn"
	// PersistedHostIDFile is the hostid the CSI node plugin persists across
	// restarts, from which its postStart hook writes DefaultHostNQNFile.
	PersistedHostIDFile = "/var/lib/nvme/hostid"
)

// NVMeHost composes the NVMe-spec host NQN for a hostid:
//
//	nqn.2014-08.org.nvmexpress:uuid:<hostID>
func NVMeHost(hostID string) string {
	return NVMeHostPrefix + ":" + uuidMarker + ":" + hostID
}

// DefaultHost returns the host NQN nvme-cli presents on this node when a
// connect names none, reading files through read (os.ReadFile in production).
//
// DefaultHostNQNFile is authoritative when it holds an NQN. The postStart hook
// that writes it may not have run yet when the CSI node plugin starts, so the
// persisted hostid it is built from is the fallback: both give the same NQN.
// With neither there is no default, and an error is returned rather than an
// invented NQN that no connect would ever present.
func DefaultHost(read func(string) ([]byte, error)) (string, error) {
	if b, err := read(DefaultHostNQNFile); err == nil {
		if v := strings.TrimSpace(string(b)); strings.HasPrefix(v, "nqn.") {
			return v, nil
		}
	}
	b, err := read(PersistedHostIDFile)
	if err != nil {
		return "", fmt.Errorf("read the node's NVMe host id: %w", err)
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", fmt.Errorf("the node's NVMe host id in %s is empty", PersistedHostIDFile)
	}
	return NVMeHost(id), nil
}
