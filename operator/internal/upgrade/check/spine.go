// §18's validation of the ownership spine, and §20's precondition that the
// reparenting has somewhere to put every dependent.
//
// They are two checks because they refuse for different reasons. The first is
// about a cluster that does not describe itself consistently, which is a state
// the migration cannot interpret. The second is about a cluster that describes
// itself perfectly well and holds something the target model has no owner for,
// which is a gap in this tool rather than in the cluster, and which a user
// resolves by telling somebody rather than by editing an object.

package check

import (
	"context"
	"fmt"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/spine"
)

// The identities of the spine checks.
const (
	IDOwnershipSpine  upgrade.ID = "ownership-spine"
	IDReparentingSafe upgrade.ID = "reparenting-is-safe"
)

// Spine returns the checks that read the ownership spine.
func Spine() []upgrade.Check {
	return []upgrade.Check{ownershipSpine(), reparentingSafe()}
}

// ownershipSpine is §18's structural validation: every referenced resource
// exists, every StorageNode belongs to the set it says it does, there are no
// unexpected owners, and no relationship is duplicated.
//
// Every one of these is a state the migration would act on wrongly rather than
// fail on. A set whose cluster does not exist has nowhere to reparent to, a
// node whose declared set and owning set disagree gets reparented onto whichever
// of the two the code happened to read, and a node two sets own gets reparented
// twice.
func ownershipSpine() upgrade.Check {
	return upgrade.CheckFunc{
		RuleID:  IDOwnershipSpine,
		Summary: "the StorageCluster, StorageNodeSet, and StorageNode spine is consistent with itself",
		RunIn:   everyStage,
		Fn: func(_ context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
			graph := spine.Build(s)
			s.Report.Work(len(graph.Sets) + len(graph.Nodes))

			findings := make(upgrade.Findings, 0, len(graph.Sets))
			for _, set := range graph.Sets {
				s.Report.Item(set.Ref.String())
				findings = append(findings, setFindings(set)...)
			}
			for _, node := range graph.Nodes {
				s.Report.Item(node.Ref.String())
				findings = append(findings, nodeFindings(node)...)
			}
			return findings, nil
		},
	}
}

// setFindings reports what is wrong with one StorageNodeSet.
func setFindings(set *spine.NodeSet) upgrade.Findings {
	if set.Cluster != nil {
		return nil
	}

	summary := fmt.Sprintf("%s names a StorageCluster that does not exist", set.Ref)
	detail := fmt.Sprintf("spec.clusterName = %s", set.ClusterName)
	if set.ClusterName == "" {
		summary = fmt.Sprintf("%s names no StorageCluster at all", set.Ref)
		detail = "spec.clusterName is empty"
	}

	return upgrade.Findings{{
		Rule:     IDOwnershipSpine,
		Severity: upgrade.SeverityError,
		Objects:  []upgrade.ObjectRef{set.Ref},
		Summary:  summary,
		Detail: detail + "\n§16.1 makes the cluster the parent of everything this set holds, " +
			"so a set with no cluster has nowhere to reparent to",
		Remediation: "point spec.clusterName at a StorageCluster in this namespace, or delete the set",
	}}
}

// nodeFindings reports what is wrong with one StorageNode.
func nodeFindings(node *spine.Node) upgrade.Findings {
	var findings upgrade.Findings

	if node.OwningSets > 1 {
		findings = append(findings, upgrade.Finding{
			Rule:     IDOwnershipSpine,
			Severity: upgrade.SeverityError,
			Objects:  []upgrade.ObjectRef{node.Ref},
			Summary:  fmt.Sprintf("%s is owned by %d StorageNodeSets", node.Ref, node.OwningSets),
			Detail: "the reparenting of §20 moves one owner reference per node, and a node " +
				"with two would be moved twice and keep whichever it was moved to last",
			Remediation: "remove the owner reference that should not be there",
		})
	}

	if len(node.ForeignOwners) > 0 {
		owners := make([]string, 0, len(node.ForeignOwners))
		for _, owner := range node.ForeignOwners {
			owners = append(owners, fmt.Sprintf("%s %s/%s", owner.Kind, owner.Namespace, owner.Name))
		}
		findings = append(findings, upgrade.Finding{
			Rule:     IDOwnershipSpine,
			Severity: upgrade.SeverityError,
			Objects:  []upgrade.ObjectRef{node.Ref},
			Summary:  fmt.Sprintf("%s has an owner that is not a StorageNodeSet", node.Ref),
			Detail: fmt.Sprintf("owned by %s\nthe reparenting knows what to do with a "+
				"StorageNodeSet owner and nothing else", joinLines(owners)),
			Remediation: "remove the owner reference, or say what the migration should do with it",
		})
	}

	switch {
	case node.Controller == nil && node.OwningSets == 0:
		findings = append(findings, upgrade.Finding{
			Rule:     IDOwnershipSpine,
			Severity: upgrade.SeverityError,
			Objects:  []upgrade.ObjectRef{node.Ref},
			Summary:  fmt.Sprintf("%s is owned by no StorageNodeSet", node.Ref),
			Detail: fmt.Sprintf("spec.storageNodeSetRef = %s\nthe controller reference is the "+
				"edge §20 moves, and there is none to move", node.DeclaredSet),
			Remediation: "let the set's controller adopt the node, or delete it",
		})

	case node.Controller != nil && node.Controller.Ref.Name != node.DeclaredSet:
		findings = append(findings, upgrade.Finding{
			Rule:     IDOwnershipSpine,
			Severity: upgrade.SeverityError,
			Objects:  []upgrade.ObjectRef{node.Ref, node.Controller.Ref},
			PerObject: []string{
				"says it belongs to " + node.DeclaredSet,
				"is what actually owns it",
			},
			Summary: fmt.Sprintf("%s belongs to one StorageNodeSet and is owned by another", node.Ref),
			Detail: "the migration would reparent it onto the cluster of whichever of the two it read, " +
				"and the two need not be the same cluster",
			Remediation: "make spec.storageNodeSetRef and the controller reference agree",
		})
	}

	return findings
}

// reparentingSafe is §20's precondition: every object a retiring StorageNodeSet
// holds has somewhere to go, so that deleting the set garbage-collects nothing
// that has to survive.
//
// An unclassified dependent is the finding this exists for, and it is the same
// refusal §12.2 makes about the Helm release. Neither disposition is safe as a
// default: reparenting an object the target model has no owner for leaves an
// orphan nothing reconciles, and leaving it behind hands it to garbage
// collection the moment the set goes.
//
// It sees the kinds discovery reads and no others. An object of some other kind
// owned by a StorageNodeSet is invisible here, because the graph learns an
// ownership edge from the dependent rather than from the owner and nothing asks
// the API server what points at a set. That is a real gap and a narrow one:
// every kind the set's own controller creates is discovered, so reaching it
// takes a second controller or a person attaching an owner reference by hand.
// Closing it means enumerating every served kind in the namespace, which is a
// discovery change rather than a check change.
func reparentingSafe() upgrade.Check {
	return upgrade.CheckFunc{
		RuleID:  IDReparentingSafe,
		Summary: "every object a retiring StorageNodeSet holds has a new owner to move to",
		RunIn:   everyStage,
		Fn: func(_ context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
			graph := spine.Build(s)
			s.Report.Work(len(graph.Sets))

			var findings upgrade.Findings
			for _, set := range graph.Sets {
				s.Report.Item(set.Ref.String())
				for _, dependent := range set.Dependents {
					if dependent.Rule != nil {
						continue
					}
					findings = append(findings, unclassified(set, dependent))
				}
			}
			return findings, nil
		},
	}
}

// unclassified renders a dependent no rule covers.
func unclassified(set *spine.NodeSet, dependent spine.Dependent) upgrade.Finding {
	survives := "deleting the set would garbage-collect it"
	if dependent.OtherOwners > 0 {
		survives = fmt.Sprintf("it has %d other owner(s), so it would survive the set's "+
			"deletion and belong to nothing this migration knows about", dependent.OtherOwners)
	}

	return upgrade.Finding{
		Rule:      IDReparentingSafe,
		Severity:  upgrade.SeverityError,
		Objects:   []upgrade.ObjectRef{dependent.Ref, set.Ref},
		PerObject: []string{"is owned by the set and has no disposition", "is the set being retired"},
		Summary: fmt.Sprintf("%s is owned by a retiring StorageNodeSet and no rule says what becomes of it",
			dependent.Ref),
		Detail: survives + "\nneither disposition is safe as a default, so the migration refuses " +
			"rather than guessing",
		Remediation: "add the kind to spine.Rules with the disposition it should have",
	}
}

// joinLines renders a list one entry per line, for a finding's detail.
func joinLines(lines []string) string {
	out := ""
	for i, line := range lines {
		if i > 0 {
			out += "\n"
		}
		out += line
	}
	return out
}
