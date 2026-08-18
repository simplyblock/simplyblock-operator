// Package cpsim is an in-memory stand-in for the simplyblock control plane.
//
// It answers the endpoints the volume-attach path reads, in the wire format the
// real one uses: the models are generated from shared/openapi.json (see gen.go),
// and the /connect entries are assembled the way sbcli assembles them in
// simplyblock_core/utils/nvme.py.
//
// Its state is set by the test rather than provisioned through the API. An
// integration test wants a fabric it decided the shape of — which volume is HA,
// which node is the primary, which endpoint is missing a namespace — and the
// real control plane reaches those states through a storage cluster this has no
// way to have.
//
// The point is that the answers can be made to agree with a fabric that really
// exists. Register the nvmet targets package fabric stood up as a volume's
// nodes, and a client resolving that volume is told to connect to them.
package cpsim

import "github.com/google/uuid"

// Cluster is what the connect path reads off a cluster.
type Cluster struct {
	ID uuid.UUID

	// QpairCount becomes nr-io-queues (cluster.client_qpair_count).
	QpairCount int

	// DataNIC becomes host-iface (cluster.client_data_nic). Empty omits it.
	DataNIC string
}

// NIC is one of a storage node's data NICs. The connect path emits one entry per
// NIC per node, so a two-NIC node is two paths to the same namespace.
type NIC struct {
	IP string

	// TrType is the transport as the control plane records it, e.g. "TCP". A
	// volume gets this transport when it matches the volume's own fabric, and
	// tcp otherwise.
	TrType string
}

// StorageNode hosts volumes and advertises the addresses they answer on.
type StorageNode struct {
	ID        uuid.UUID
	ClusterID uuid.UUID
	Hostname  string

	// MgmtIP must be an IPv4 address: the client validates it as one.
	MgmtIP string

	// Status is free text on purpose — a newer control plane may invent one, and
	// the client deliberately does not check membership.
	Status string

	DataNICs []NIC
}

// Pool groups volumes within a cluster.
type Pool struct {
	ID        uuid.UUID
	ClusterID uuid.UUID
	Name      string

	// MaxSize 0 means unlimited, as it does in the control plane.
	MaxSize int
}

// AllowedHost is one entry of a volume's access-control list. A volume with any
// of these restricts connects to the hosts named here and resolves each one's
// key material; a volume with none allows any host.
type AllowedHost struct {
	NQN string

	// DHCHAPKey and DHCHAPCtrlKey appear only inside the prebuilt connect
	// command, which is also the only place the real control plane puts them.
	DHCHAPKey     string
	DHCHAPCtrlKey string

	// PSK non-empty sets tls on the entry.
	PSK string
}

// Volume is a logical volume and the paths to it.
type Volume struct {
	ID        uuid.UUID
	PoolID    uuid.UUID
	ClusterID uuid.UUID

	Name     string
	PoolName string

	// NQN is the subsystem NQN. The client requires it to start with "nqn.".
	NQN string

	// NSID is the namespace id within that subsystem. The client requires
	// /connect to report it greater than zero — that promise is the endpoint's,
	// and the rename it guards against is exactly what this simulator could
	// otherwise reintroduce unnoticed.
	NSID int

	// SizeBytes must be non-zero: the client rejects a volume of size 0.
	SizeBytes uint64

	// Fabric is the transport the volume was created for, e.g. "tcp".
	Fabric string

	// Port is the subsystem's client-facing port. One value for the whole
	// volume, not one per node: every node hosting an lvstore serves it on the
	// same port, and the real control plane reads it off the primary.
	Port int

	// Nodes are the nodes serving this volume, primary first. One node is a
	// single-replica volume; more is an HA volume, and the order is the order
	// the connect entries come back in.
	Nodes []uuid.UUID

	AllowedHosts []AllowedHost
}

// HighAvailability reports whether the volume has more than one node, which is
// what ha_type means in the control plane.
func (v Volume) HighAvailability() bool { return len(v.Nodes) > 1 }

// allowedHost finds a volume's ACL entry for a host NQN.
func (v Volume) allowedHost(nqn string) (AllowedHost, bool) {
	for _, h := range v.AllowedHosts {
		if h.NQN == nqn {
			return h, true
		}
	}
	return AllowedHost{}, false
}

// allowedHostNQNs is the ACL as the wire carries it.
func (v Volume) allowedHostNQNs() []string {
	nqns := make([]string, 0, len(v.AllowedHosts))
	for _, h := range v.AllowedHosts {
		nqns = append(nqns, h.NQN)
	}
	return nqns
}
