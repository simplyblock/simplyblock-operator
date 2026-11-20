// The node-set builder that splits a draft by what its machines are for.
//
// It is the default rather than SingleNodeSet because the split is what makes the
// role actionable. On a fleet with disks in both its infrastructure nodes and its
// workers, the infra nodes are almost always the ones somebody meant to be the
// storage: simplyblock storage is infrastructure, and on OpenShift those nodes do
// not count against a subscription's core limit. Proposing them first, in a block
// of their own, lets a reviewer take that placement by deleting the other block
// rather than by moving hostnames between them.
//
// A fleet with no infrastructure tier gets exactly what SingleNodeSet produced:
// one set named for what it is, holding every worker.

package discovery

import (
	"sort"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// SplitByRole puts each role's machines in a node set of its own, ordered so the
// infrastructure tier comes first.
type SplitByRole struct {
	// SetName overrides the name of the worker set. The infrastructure and
	// control-plane sets are named for their roles, because a reviewer deleting
	// one of those is choosing a tier rather than a rack.
	SetName string
}

func (SplitByRole) Name() string { return "one node set per role" }

func (b SplitByRole) Build(groups []Group) []simplyblockv1alpha2.NodeSet {
	if len(groups) == 0 {
		return nil
	}

	// A group is the machines that hand over the same devices, and the grouper
	// that produced it knew nothing about roles, so one group may hold machines
	// of two. Splitting here rather than grouping by role first keeps the
	// hardware grouping intact: two infra nodes with identical disks stay one
	// group.
	byRole := map[NodeRole][]Group{}
	for _, group := range groups {
		for role, workers := range splitWorkersByRole(group.Workers) {
			split := group
			split.Workers = workers
			byRole[role] = append(byRole[role], split)
		}
	}

	roles := make([]NodeRole, 0, len(byRole))
	for role := range byRole {
		roles = append(roles, role)
	}
	sort.Slice(roles, func(i, j int) bool {
		if roles[i].Preference() != roles[j].Preference() {
			return roles[i].Preference() < roles[j].Preference()
		}
		return roles[i] < roles[j]
	})

	out := make([]simplyblockv1alpha2.NodeSet, 0, len(roles))
	for _, role := range roles {
		name := role.NodeSetName()
		if role == NodeRoleWorker && b.SetName != "" {
			name = b.SetName
		}
		set := simplyblockv1alpha2.NodeSet{
			Name:   name,
			Groups: make([]simplyblockv1alpha2.NodeGroup, 0, len(byRole[role])),
		}
		for _, group := range byRole[role] {
			set.Groups = append(set.Groups, nodeGroupOf(group))
		}
		out = append(out, set)
	}
	return out
}

// splitWorkersByRole divides one hardware group's machines by what they are for,
// keeping each slice in the order the group had them.
func splitWorkersByRole(workers []Worker) map[NodeRole][]Worker {
	out := map[NodeRole][]Worker{}
	for _, worker := range workers {
		role := worker.Kube.Role
		if role == "" {
			// The planner was given no node objects, so nothing said what the
			// machine is, and a machine with no role label is a worker.
			role = NodeRoleWorker
		}
		out[role] = append(out[role], worker)
	}
	return out
}
