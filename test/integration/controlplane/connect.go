package cpsim

import (
	"fmt"
	"strings"

	"github.com/simplyblock/atlas/ptr"
)

// The tunables the control plane puts on every connect entry
// (sbcli simplyblock_core/constants.py). They are values a client reads and acts
// on, so a stand-in that invents its own would be testing against a fabric
// nobody runs.
const (
	reconnectDelaySec = 2
	ctrlLossTMOSec    = 60 * 60
	fastIOFailTMOSec  = 8

	// Keep-alive is shorter over tcp than over rdma.
	keepAliveTMOTCPSec   = 4
	keepAliveTMOOtherSec = 7
)

// connectEntries renders the volume's paths, one per data NIC per node, in node
// order — primary first.
//
// This mirrors lvol_controller.connect_lvol and build_nvme_connect_entry: the
// transport is the NIC's when it matches the volume's fabric and tcp otherwise,
// every entry carries the same port, and the namespace id is the volume's.
func (s *Server) connectEntries(v Volume, hostNQN string) ([]NvmeConnectEntry, error) {
	auth, err := s.resolveHost(v, hostNQN)
	if err != nil {
		return nil, err
	}

	cluster, ok := s.clusters[v.ClusterID]
	if !ok {
		return nil, fmt.Errorf("volume %s names cluster %s, which is not registered", v.ID, v.ClusterID)
	}

	var entries []NvmeConnectEntry
	for _, nodeID := range v.Nodes {
		node, ok := s.nodes[nodeID]
		if !ok {
			return nil, fmt.Errorf("volume %s names node %s, which is not registered", v.ID, nodeID)
		}
		for _, nic := range node.DataNICs {
			entries = append(entries, buildConnectEntry(v, cluster, nic, auth, hostNQN))
		}
	}
	return entries, nil
}

// resolveHost authorizes a connecting host and returns its key material.
//
// nil means the volume allows any host. The two errors are the real ones, text
// included: a client that special-cases them by message has to keep working.
func (s *Server) resolveHost(v Volume, hostNQN string) (*AllowedHost, error) {
	if len(v.AllowedHosts) == 0 {
		return nil, nil
	}
	if hostNQN == "" {
		return nil, hostError(fmt.Sprintf(
			"Volume %s has allowed hosts configured; --host-nqn is required", v.ID))
	}
	h, ok := v.allowedHost(hostNQN)
	if !ok {
		return nil, hostError(fmt.Sprintf(
			"Host NQN %s not found in allowed hosts for volume %s", hostNQN, v.ID))
	}
	return &h, nil
}

// buildConnectEntry is build_nvme_connect_entry.
func buildConnectEntry(v Volume, c Cluster, nic NIC, auth *AllowedHost, hostNQN string) NvmeConnectEntry {
	transport := "tcp"
	if nic.IP != "" && strings.EqualFold(nic.TrType, v.Fabric) {
		transport = strings.ToLower(nic.TrType)
	}
	keepAlive := keepAliveTMOOtherSec
	if transport == "tcp" {
		keepAlive = keepAliveTMOTCPSec
	}

	tls := auth != nil && auth.PSK != ""

	return NvmeConnectEntry{
		NsId:           ptr.To(v.NSID),
		Transport:      transport,
		Ip:             nic.IP,
		Port:           v.Port,
		Nqn:            v.NQN,
		ReconnectDelay: reconnectDelaySec,
		CtrlLossTmo:    ctrlLossTMOSec,
		FastIoFailTmo:  fastIOFailTMOSec,
		NrIoQueues:     c.QpairCount,
		KeepAliveTmo:   keepAlive,
		HostIface:      ptr.To(c.DataNIC),
		Tls:            ptr.To(tls),
		AllowedHosts:   ptr.To(v.allowedHostNQNs()),
		Connect: connectCommand(connectCmdArgs{
			transport: transport,
			ip:        nic.IP,
			port:      v.Port,
			nqn:       v.NQN,
			keepAlive: keepAlive,
			qpairs:    c.QpairCount,
			dataNIC:   c.DataNIC,
			tls:       tls,
			hostNQN:   hostNQN,
			auth:      auth,
		}),
	}
}

type connectCmdArgs struct {
	transport string
	ip        string
	port      int
	nqn       string
	keepAlive int
	qpairs    int
	dataNIC   string
	tls       bool
	hostNQN   string
	auth      *AllowedHost
}

// connectCommand renders the prebuilt "nvme connect" command line.
//
// Worth rendering rather than stubbing: it is the only place the response
// carries the DHCHAP secrets — they have no fields of their own — so a client
// that needs them parses them back out of this string.
func connectCommand(a connectCmdArgs) string {
	var host strings.Builder
	if a.hostNQN != "" {
		fmt.Fprintf(&host, " --hostnqn=%s", a.hostNQN)
	}
	if a.auth != nil {
		if a.auth.DHCHAPKey != "" {
			fmt.Fprintf(&host, " --dhchap-secret=%s", a.auth.DHCHAPKey)
		}
		if a.auth.DHCHAPCtrlKey != "" {
			fmt.Fprintf(&host, " --dhchap-ctrl-secret=%s", a.auth.DHCHAPCtrlKey)
		}
	}
	iface := ""
	if a.dataNIC != "" {
		iface = "--host-iface=" + a.dataNIC
	}
	tls := ""
	if a.tls {
		tls = " --tls"
	}

	// The option spelling is the control plane's, down to fast_io_fail_tmo's
	// underscores among hyphenated neighbours.
	return fmt.Sprintf("sudo nvme connect"+
		" --reconnect-delay=%d --ctrl-loss-tmo=%d --fast_io_fail_tmo=%d"+
		" --nr-io-queues=%d --keep-alive-tmo=%d"+
		" --transport=%s --traddr=%s --trsvcid=%d --nqn=%s"+
		" %s%s%s",
		reconnectDelaySec, ctrlLossTMOSec, fastIOFailTMOSec,
		a.qpairs, a.keepAlive,
		a.transport, a.ip, a.port, a.nqn,
		iface, tls, host.String())
}
