// §18's in-flight checks, which the design calls the ones that matter most.
//
// An operation the operator is partway through is state that lives in an object
// this migration is about to rewrite, and finishing it takes minutes while
// migrating it takes a design. So the migration refuses rather than translating,
// and the remediation is always the same: wait, or cancel it.

package check

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/runtime/schema"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// The identities of the in-flight checks.
const (
	IDOperationsInFlight upgrade.ID = "operations-in-flight"
	IDNodesOnline        upgrade.ID = "storage-nodes-online"
)

// InFlight returns the checks about work the operator has not finished.
func InFlight() []upgrade.Check {
	return []upgrade.Check{operationsInFlight(), nodesOnline()}
}

// terminal lists the phase values that mean an operation has stopped. They are
// compared case-insensitively against whichever field the kind keeps its phase
// in, because the kinds do not agree on the spelling: §7.2 recases three enums
// and the drain workflow uses lower case with underscores.
var terminal = []string{"succeeded", "failed", "completed", "aborted", "complete", "cancelled", "canceled"}

// isTerminal reports whether a phase value means the operation has stopped. An
// empty phase is not terminal: an object the operator has not reached yet is
// one it is about to start.
func isTerminal(phase string) bool {
	return slices.Contains(terminal, lower(phase))
}

// lower is strings.ToLower without the import, kept beside its one caller.
func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + ('a' - 'A')
		}
	}
	return string(out)
}

// operationsInFlight refuses while any of the four operation kinds is running,
// and while a StorageNodeSet is partway through a drain.
//
// The drain is the one worth naming separately. §16.1 records that
// status.drainCoordination carries an eight-phase workflow driven by a
// controller of its own, and that it becomes a StorageNodeOps action. A drain
// in progress when the migration starts is a state the migration refuses to
// migrate, and it is a validation failure rather than something to translate.
func operationsInFlight() upgrade.Check {
	return upgrade.CheckFunc{
		RuleID:  IDOperationsInFlight,
		Summary: "no cluster operation, node operation, volume migration, backup restore, or drain is in flight",
		RunIn:   everyStage,
		Fn: func(_ context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
			s.Report.Work(5)

			var findings upgrade.Findings

			s.Report.Item("StorageClusterOps")
			for _, ops := range upgrade.Typed[*simplyblockv1alpha1.StorageClusterOps](s.Graph, gvkOf("StorageClusterOps")) {
				if !isTerminal(string(ops.Status.Phase)) {
					findings = append(findings, running(s.Ref(ops), string(ops.Status.Phase),
						"a cluster operation the migration would rewrite mid-flight"))
				}
			}

			s.Report.Item("StorageNodeOps")
			for _, ops := range upgrade.Typed[*simplyblockv1alpha1.StorageNodeOps](s.Graph, gvkOf("StorageNodeOps")) {
				if !isTerminal(string(ops.Status.Phase)) {
					findings = append(findings, running(s.Ref(ops), string(ops.Status.Phase),
						"a node operation the migration would rewrite mid-flight"))
				}
			}

			s.Report.Item("VolumeMigration")
			for _, migration := range upgrade.Typed[*simplyblockv1alpha1.VolumeMigration](s.Graph, gvkOf("VolumeMigration")) {
				if !isTerminal(string(migration.Status.Phase)) {
					findings = append(findings, running(s.Ref(migration), string(migration.Status.Phase),
						"§16.2 copies this kind into a cluster-scoped PersistentVolumeOps, and one still "+
							"running is not copied mid-flight"))
				}
			}

			s.Report.Item("BackupRestore")
			for _, restore := range upgrade.Typed[*simplyblockv1alpha1.BackupRestore](s.Graph, gvkOf("BackupRestore")) {
				if !isTerminal(restore.Status.Phase) {
					findings = append(findings, running(s.Ref(restore), restore.Status.Phase,
						"§16.2 absorbs this kind into a StorageBackupOps action, and one still "+
							"running is not absorbed mid-flight"))
				}
			}

			s.Report.Item("StorageNodeSet drains")
			for _, set := range upgrade.Typed[*simplyblockv1alpha1.StorageNodeSet](s.Graph, gvkOf("StorageNodeSet")) {
				findings = append(findings, draining(s, set)...)
			}
			return findings, nil
		},
	}
}

// draining reports the workers a set is partway through draining.
func draining(s *upgrade.Scope, set *simplyblockv1alpha1.StorageNodeSet) upgrade.Findings {
	var findings upgrade.Findings
	for _, state := range set.Status.DrainCoordination {
		if isTerminal(state.Phase) {
			continue
		}
		findings = append(findings, upgrade.Finding{
			Rule:     IDOperationsInFlight,
			Severity: upgrade.SeverityError,
			Objects:  []upgrade.ObjectRef{s.Ref(set)},
			Summary: fmt.Sprintf("%s is draining %s, which is at phase %q",
				s.Ref(set), state.Hostname, state.Phase),
			Detail: "the drain workflow becomes a StorageNodeOps action, and one in progress " +
				"is a state the migration refuses to translate",
			Remediation: "let the drain finish, or cancel it, before running the migration",
		})
	}
	return findings
}

// running renders one unfinished operation.
func running(ref upgrade.ObjectRef, phase, why string) upgrade.Finding {
	if phase == "" {
		phase = "not started"
	}
	return upgrade.Finding{
		Rule:        IDOperationsInFlight,
		Severity:    upgrade.SeverityError,
		Objects:     []upgrade.ObjectRef{ref},
		PerObject:   []string{"phase " + phase},
		Summary:     fmt.Sprintf("%s has not finished", ref),
		Detail:      why,
		Remediation: "let the operation finish, or delete it, before running the migration",
	}
}

// nodesOnline refuses while a StorageNode is not online, unless the run says it
// knows.
//
// It is separate from the operations because the remediation is different and
// so is the severity of getting it wrong. An offline node is a cluster that is
// already degraded, and §9.2 says a cluster that is degraded should not be asked
// to absorb an upgrade. But an operator who knows a node is offline and intends
// to upgrade anyway is making a legitimate call, which is what the
// acknowledgment is for.
func nodesOnline() upgrade.Check {
	return upgrade.CheckFunc{
		RuleID:  IDNodesOnline,
		Summary: "every StorageNode is online, or the run acknowledges that one is not",
		RunIn:   everyStage,
		Fn: func(_ context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
			nodes := upgrade.Typed[*simplyblockv1alpha1.StorageNode](s.Graph, gvkOf("StorageNode"))
			s.Report.Work(len(nodes))

			var findings upgrade.Findings
			for _, node := range nodes {
				s.Report.Item(node.Name)
				if lower(node.Status.Status) == "online" {
					continue
				}

				severity := upgrade.SeverityError
				remediation := "bring the node back online, or rerun with --acknowledge-offline"
				if s.Options.AcknowledgeOffline {
					severity = upgrade.SeverityWarning
					remediation = "acknowledged on the command line, and the migration will proceed"
				}

				status := node.Status.Status
				if status == "" {
					status = "unreported"
				}
				findings = append(findings, upgrade.Finding{
					Rule:      IDNodesOnline,
					Severity:  severity,
					Objects:   []upgrade.ObjectRef{s.Ref(node)},
					PerObject: []string{"status " + status},
					Summary:   fmt.Sprintf("%s is not online", s.Ref(node)),
					Detail: "a cluster that is already degraded should not be asked to absorb " +
						"an upgrade, since the migration cannot tell a node that is down from " +
						"one it has just broken",
					Remediation: remediation,
				})
			}
			return findings, nil
		},
	}
}

// gvkOf names a kind in this API group and version.
func gvkOf(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "storage.simplyblock.io", Version: "v1alpha1", Kind: kind}
}
