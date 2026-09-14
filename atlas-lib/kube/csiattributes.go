// The volume attributes a PersistentVolume carries for the simplyblock CSI
// driver, built from what the control plane says about a volume's connection.
//
// It lives here because the producer and the consumer are in different
// components: the operator writes a PersistentVolume when it restores a backup,
// and the CSI driver's node service reads exactly these keys back out of it. A
// key spelled one way on the write side and another on the read side produces a
// volume that provisions and will not mount, which is the failure that only
// shows up on a real cluster.
//
// The CSI driver builds an equivalent map of its own in
// internal/controlplane/client.go, and this is deliberately not that function
// yet. That one also carries a model derived from the subsystem NQN and the
// target lvol id a failover produces, and neither is on lvol.Connection, so
// unifying the two means widening that type first. Until then this is the
// spelling of record and the other is the one to bring across.

package kube

import (
	"encoding/json"
	"strconv"

	"github.com/simplyblock/atlas/lvol"
)

// The keys the CSI node service reads. They are constants rather than literals
// so that the write side and any future read side name one thing.
const (
	CSIAttrName           = "name"
	CSIAttrUUID           = "uuid"
	CSIAttrModel          = "model"
	CSIAttrNQN            = "nqn"
	CSIAttrNSID           = "nsId"
	CSIAttrTargetType     = "targetType"
	CSIAttrConnections    = "connections"
	CSIAttrReconnectDelay = "reconnectDelay"
	CSIAttrNrIOQueues     = "nrIoQueues"
	CSIAttrCtrlLossTMO    = "ctrlLossTmo"
	CSIAttrHostIface      = "hostIface"
	CSIAttrClusterID      = "cluster_id"
)

// csiEndpoint is one reachable address, in the shape the node service decodes
// the connections attribute into. Only the address and the port travel: the
// per-path tunables are single values on the attribute map, because the node
// service applies one set to every path.
type csiEndpoint struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// CSIVolumeAttributes renders a volume's connection into the attribute map a
// PersistentVolume carries for the simplyblock CSI driver.
//
// The per-path tunables come from the first endpoint, which is the primary: the
// control plane returns the paths in its own priority order, and the node
// service applies one set of tunables to all of them. A connection with no
// endpoints yields a map with no connections entry rather than an error, because
// what to do about an unreachable volume is the caller's decision and not this
// function's.
func CSIVolumeAttributes(handle lvol.VolumeHandle, conn lvol.Connection) (map[string]string, error) {
	clusterID, _, volumeID, err := handle.Split()
	if err != nil {
		return nil, err
	}

	attrs := map[string]string{
		CSIAttrClusterID: clusterID.String(),
		CSIAttrName:      volumeID.String(),
		CSIAttrUUID:      volumeID.String(),
		CSIAttrModel:     volumeID.String(),
		CSIAttrNQN:       conn.NQN,
		CSIAttrNSID:      strconv.FormatUint(uint64(conn.NSID), 10),
	}
	if conn.UUID != "" {
		// The namespace's own identity, which after a failover is the clone's
		// rather than the volume's. It is what a device lookup falls back to
		// when the subsystem coordinates no longer find the volume.
		attrs[CSIAttrUUID] = conn.UUID
	}
	if len(conn.Endpoints) == 0 {
		return attrs, nil
	}

	endpoints := make([]csiEndpoint, 0, len(conn.Endpoints))
	for _, endpoint := range conn.Endpoints {
		endpoints = append(endpoints, csiEndpoint{IP: endpoint.Address, Port: endpoint.Port})
	}
	encoded, err := json.Marshal(endpoints)
	if err != nil {
		return nil, err
	}

	primary := conn.Endpoints[0]
	attrs[CSIAttrConnections] = string(encoded)
	attrs[CSIAttrTargetType] = primary.Transport
	attrs[CSIAttrReconnectDelay] = strconv.Itoa(primary.ReconnectDelaySec)
	attrs[CSIAttrNrIOQueues] = strconv.Itoa(primary.NrIOQueues)
	if primary.CtrlLossTMOSec != nil {
		attrs[CSIAttrCtrlLossTMO] = strconv.Itoa(*primary.CtrlLossTMOSec)
	}
	if primary.HostIface != "" {
		attrs[CSIAttrHostIface] = primary.HostIface
	}
	return attrs, nil
}
