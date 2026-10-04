// What a node is for, read off the labels a distribution puts on it.
//
// A storage node is an SPDK process holding a machine's disks, and which machines
// are meant to hold disks is a question about the cluster's own topology rather
// than about the hardware.
//
// The roles do not rank the way a first guess suggests. An OpenShift
// infrastructure node is the tier a cluster's own infrastructure runs on, and
// simplyblock storage is infrastructure: a fleet with disks in its infra nodes
// almost always meant those disks to be the storage, and on OpenShift they are
// also the nodes that do not count against a subscription's core limit. So an
// infra node with disks is preferred over a worker with disks rather than avoided.
//
// A control-plane node is the opposite. It runs the API server and etcd, and a
// data path on an etcd host is a placement almost nobody intends, except in the
// combined three-node and single-node deployments this product supports, which is
// what the opt-in exists for.
//
// The role is a label rather than a taint, and that distinction is why this file
// exists at all. A taint is the cluster refusing to schedule there, which the
// worker selection already honors. A label is the cluster saying what the machine
// is for, and the two do not coincide: Kubernetes taints its control-plane nodes
// and OpenShift usually does not taint its infrastructure ones, so a fleet's infra
// nodes pass every taint check and were invisible to this operator.
//
// The label keys are Kubernetes' own convention rather than this product's, so
// they keep their spelling: node-role.kubernetes.io/<role> is what every
// distribution writes and what `kubectl get nodes` prints in its ROLES column.

package discovery

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// NodeRole is what a machine is for.
type NodeRole string

const (
	// NodeRoleWorker is an ordinary worker, which is what a node carrying no role
	// label at all also is. It holds storage nodes, and is what a fleet with no
	// infrastructure tier is made of.
	NodeRoleWorker NodeRole = "Worker"

	// NodeRoleControlPlane runs the API server and etcd. Kubernetes taints these
	// by default, so most fleets exclude them twice over, but a combined
	// three-node or single-node deployment deliberately removes that taint and is
	// the case the opt-in exists for.
	NodeRoleControlPlane NodeRole = "ControlPlane"

	// NodeRoleInfra is OpenShift's infrastructure role: the registry, the router,
	// and the monitoring stack. It is the role this file is really about, in two
	// ways. OpenShift does not always taint it, so nothing else in the operator
	// would have noticed it; and it is the tier storage belongs on, so noticing it
	// is what lets a draft propose the placement a fleet intended.
	NodeRoleInfra NodeRole = "Infra"
)

// roleLabelPrefix is the convention every distribution writes a node's role
// under, and the one `kubectl get nodes` reads its ROLES column from.
const roleLabelPrefix = "node-role.kubernetes.io/"

// The role names that appear after the prefix. `master` is the older spelling of
// `control-plane` and is still written by clusters installed before it changed, so
// both are read and neither is preferred.
const (
	roleNameControlPlane = "control-plane"
	roleNameMaster       = "master"
	roleNameInfra        = "infra"
	roleNameWorker       = "worker"
)

// RoleOf is what the node is for, and Worker when it says nothing.
//
// A node may carry several role labels at once, which is how a combined
// deployment marks a machine that is both a control-plane node and a worker. The
// most restrictive wins: a machine that runs etcd is a machine that runs etcd
// whatever else it also does, so a storage node placed there is placed on an etcd
// host either way. Infra outranks Worker for the same reason read the other way —
// a machine labeled both is part of the infrastructure tier.
func RoleOf(node corev1.Node) NodeRole {
	roles := RolesOf(node)
	for _, role := range []NodeRole{NodeRoleControlPlane, NodeRoleInfra} {
		for _, held := range roles {
			if held == role {
				return role
			}
		}
	}
	return NodeRoleWorker
}

// RolesOf is every role the node's labels name, so a draft can report what a
// machine is rather than only what it was reduced to.
func RolesOf(node corev1.Node) []NodeRole {
	seen := map[NodeRole]struct{}{}
	for key := range node.Labels {
		name, found := strings.CutPrefix(key, roleLabelPrefix)
		if !found {
			continue
		}
		switch name {
		case roleNameControlPlane, roleNameMaster:
			seen[NodeRoleControlPlane] = struct{}{}
		case roleNameInfra:
			seen[NodeRoleInfra] = struct{}{}
		case roleNameWorker:
			seen[NodeRoleWorker] = struct{}{}
		default:
			// A role this product does not know is not a role it should refuse.
			// Fleets label machines for their own purposes, and a storage node
			// belongs on one of those unless somebody says otherwise.
		}
	}
	if len(seen) == 0 {
		return []NodeRole{NodeRoleWorker}
	}

	out := make([]NodeRole, 0, len(seen))
	for role := range seen {
		out = append(out, role)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// HoldsStorageNodes reports whether a role is one a storage node belongs on
// without somebody asking for it.
//
// Infra and Worker both do. ControlPlane does not, and that is the conservative
// half of a decision with a real cost: a fleet that genuinely wants storage on its
// control-plane nodes has to say so. The other way round is worse, because the
// draft a reviewer rubber-stamps would put a data path on the machines running
// etcd, and a fifty-worker document is not one anybody reads closely enough to
// catch three of them in it.
func (r NodeRole) HoldsStorageNodes() bool {
	return r == NodeRoleWorker || r == NodeRoleInfra
}

// Preference orders the roles a draft proposes, lowest first. It is what puts the
// infrastructure tier ahead of the workers on a fleet that has both, so the node
// set a reviewer reads first is the one they most likely meant.
func (r NodeRole) Preference() int {
	switch r {
	case NodeRoleInfra:
		return 0
	case NodeRoleWorker:
		return 1
	default:
		return 2
	}
}

// NodeSetName is what a draft calls the node set holding this role's machines.
//
// Splitting the draft by role rather than mixing the machines into one set is what
// makes the preference actionable: a reviewer who wants only the infrastructure
// tier deletes a block, rather than moving hostnames between them.
func (r NodeRole) NodeSetName() string {
	switch r {
	case NodeRoleInfra:
		return "infra"
	case NodeRoleControlPlane:
		return "control-plane"
	default:
		return DefaultNodeSetName
	}
}

// Describe renders the role for the note that says what a machine is.
func (r NodeRole) Describe() string {
	switch r {
	case NodeRoleControlPlane:
		return "a control-plane node, which runs the API server and etcd"
	case NodeRoleInfra:
		return "an infrastructure node, which runs the registry, the router, and monitoring"
	default:
		return "a worker"
	}
}
