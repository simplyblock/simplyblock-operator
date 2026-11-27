// Moving a FoundationDB process back onto a worker that has none of its
// class, after the processes ended up crowded on fewer workers.
//
// The placement of the database's processes is a preference (the anti-affinity
// between processes of one class in foundationdb.go), so a worker that goes
// away has its pods rescheduled onto the others, and nothing in Kubernetes
// moves them back when it returns. The worker is the database's fault domain:
// two log processes on one worker are two coordinators on one worker, and its
// loss takes the quorum the redundancy mode was configured to survive.
//
// So steady state looks for a class whose processes share a worker while an
// eligible worker runs none of it, and hands one of them to the FoundationDB
// operator to replace through the FoundationDBCluster's
// spec.processGroupsToRemove. The FoundationDB operator marks the group for
// removal, adds a replacement (which the anti-affinity places on the worker
// that has none), excludes the old process, and removes it: the data movement
// is the FoundationDB operator's, as it is for every other replacement.
//
// One replacement at a time, and only on a settled database: no operation on
// the control plane (a HostMaintenance window moves processes itself), the
// FoundationDB operator caught up with the spec, full replication, no process
// group reporting a condition, and no removal in flight.
//
// The request is written by a server-side apply under a field manager of its
// own. The install's apply of the cluster, which every steady state pass makes,
// never sets the field, so it leaves the entry alone. The FoundationDB operator
// never clears the list. The next request drops the entry by writing only its
// own ID, as kubectl-fdb's GetProcessGroupsToRemove drops IDs already removed.
//
// design-fdb-host-maintenance.md §5.13 is the specification.

package controlplane

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// fdbRebalanceFieldOwner writes spec.processGroupsToRemove and nothing
	// else. It is not fieldOwner, so the install's apply of the cluster, which
	// does not set the field, does not take it away.
	fdbRebalanceFieldOwner = client.FieldOwner("simplyblock-operator-fdb-rebalance")

	// fdbRebalanceHeldEvery is how often a held rebalance is announced again
	// while the same thing holds it. Steady state runs every thirty seconds, and
	// an event on every pass is the pathological repetition the crowding
	// itself produced.
	fdbRebalanceHeldEvery = 10 * time.Minute
)

// fdbRebalancedClasses are the classes the rebalance spreads. Both carry the
// anti-affinity that places a replacement on a worker without its class, and
// both are coordinator candidates. Stateless processes have no anti-affinity
// of their own, so a replacement would land anywhere, and they hold neither
// data nor a coordinator.
var fdbRebalancedClasses = []string{"log", "storage"}

// rebalanceGroup is what the rebalance reads of one process group.
type fdbRebalanceGroup struct {
	id, class, zone string
	removing        bool
	troubled        bool
}

// fdbRebalanceNotes remembers what held the rebalance of each namespace and
// when that was announced, so the event is not repeated every pass. Losing it
// to a restart costs one repeated event.
type fdbRebalanceNotes struct {
	mu    sync.Mutex
	notes map[string]fdbRebalanceNote
}

type fdbRebalanceNote struct {
	message string
	at      time.Time
}

// due reports whether a held rebalance is worth announcing: the first time,
// when what holds it changed, or when the last announcement is old.
func (n *fdbRebalanceNotes) due(namespace, message string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.notes == nil {
		n.notes = map[string]fdbRebalanceNote{}
	}
	last, ok := n.notes[namespace]
	if ok && last.message == message && now.Sub(last.at) < fdbRebalanceHeldEvery {
		return false
	}
	n.notes[namespace] = fdbRebalanceNote{message: message, at: now}
	return true
}

func (n *fdbRebalanceNotes) clear(namespace string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.notes, namespace)
}

// rebalanceFoundationDB asks the FoundationDB operator to replace one process
// of a crowded class, or does nothing. It never waits: steady state calls it
// on every pass.
func (r *ControlPlaneReconciler) rebalanceFoundationDB(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) error {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(fdbClusterGVK)
	key := client.ObjectKey{Namespace: cp.Namespace, Name: ComponentFDBCluster}
	if err := r.Get(ctx, key, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read FoundationDBCluster %s: %w", ComponentFDBCluster, err)
	}
	groups, err := fdbRebalanceGroups(cluster)
	if err != nil {
		return err
	}
	zones, err := r.eligibleFDBZones(ctx, cp)
	if err != nil {
		return err
	}
	candidate, class, from, to := crowdedProcess(groups, zones)
	if candidate == "" {
		r.rebalanceNotes.clear(cp.Namespace)
		return nil
	}

	held, err := r.rebalanceHold(ctx, cp, cluster, groups)
	if err != nil {
		return err
	}
	if held != "" {
		message := fmt.Sprintf("%s processes share worker %s while %s has none; not moving one yet: %s",
			class, from, to, held)
		if r.rebalanceNotes.due(cp.Namespace, message, time.Now()) {
			r.emit(cp, corev1.EventTypeNormal, FoundationDBRebalanceHeld, message)
		}
		return nil
	}

	request := &unstructured.Unstructured{}
	request.SetGroupVersionKind(fdbClusterGVK)
	request.SetName(ComponentFDBCluster)
	request.SetNamespace(cp.Namespace)
	if err := unstructured.SetNestedSlice(request.Object, []any{candidate}, "spec", "processGroupsToRemove"); err != nil {
		return err
	}
	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(request),
		fdbRebalanceFieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("ask for process group %s to be replaced: %w", candidate, err)
	}
	r.rebalanceNotes.clear(cp.Namespace)
	r.emit(cp, corev1.EventTypeNormal, FoundationDBRebalanceRequested, fmt.Sprintf(
		"%s processes share worker %s while %s has none; process group %s is replaced, "+
			"and its replacement is placed on a worker without its class", class, from, to, candidate))
	return nil
}

// fdbRebalanceGroups reads status.processGroups.
func fdbRebalanceGroups(cluster *unstructured.Unstructured) ([]fdbRebalanceGroup, error) {
	entries, _, err := unstructured.NestedSlice(cluster.Object, "status", "processGroups")
	if err != nil {
		return nil, fmt.Errorf("read status.processGroups of %s: %w", ComponentFDBCluster, err)
	}
	groups := make([]fdbRebalanceGroup, 0, len(entries))
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		g := fdbRebalanceGroup{}
		g.id, _ = m["processGroupID"].(string)
		g.class, _ = m["processClass"].(string)
		g.zone, _ = m["faultDomain"].(string)
		_, g.removing = m["removalTimestamp"]
		conditions, _ := m["processGroupConditions"].([]any)
		g.troubled = len(conditions) > 0
		groups = append(groups, g)
	}
	return groups, nil
}

// eligibleFDBZones is the fault domain, the hostname label, of every worker a
// FoundationDB pod of this control plane can be placed on: Ready, schedulable,
// matching the database's node selector, and tolerating every taint that keeps
// pods off it.
func (r *ControlPlaneReconciler) eligibleFDBZones(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) (map[string]bool, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, client.MatchingLabels(foundationDBNodeSelector(cp))); err != nil {
		return nil, fmt.Errorf("list the Nodes: %w", err)
	}
	tolerations := foundationDBTolerations(cp)
	zones := map[string]bool{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		zone := node.Labels[corev1.LabelHostname]
		if zone == "" || node.Spec.Unschedulable || !nodeReady(node) || !toleratesAll(ctx, tolerations, node.Spec.Taints) {
			continue
		}
		zones[zone] = true
	}
	return zones, nil
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// toleratesAll reports whether the tolerations cover every taint that keeps a
// new pod off the node. A PreferNoSchedule taint does not.
func toleratesAll(ctx context.Context, tolerations []corev1.Toleration, taints []corev1.Taint) bool {
	logger := logf.FromContext(ctx)
	for i := range taints {
		taint := &taints[i]
		if taint.Effect == corev1.TaintEffectPreferNoSchedule {
			continue
		}
		tolerated := false
		for j := range tolerations {
			if tolerations[j].ToleratesTaint(logger, taint, false) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

// crowdedProcess picks the process group to replace: of the first class, in
// the order fdbRebalancedClasses names them, that has two or more processes on
// one worker while an eligible worker has none, a process on the most crowded
// of its workers. Ties go to the lexically last zone and ID, so the choice is
// the same on every pass. It returns the group, its class, its worker, and a
// worker the replacement can land on, or empty strings for nothing to do.
func crowdedProcess(groups []fdbRebalanceGroup, eligible map[string]bool) (id, class, from, to string) {
	for _, class := range fdbRebalancedClasses {
		byZone := map[string][]string{}
		for _, g := range groups {
			if g.class == class && !g.removing {
				byZone[g.zone] = append(byZone[g.zone], g.id)
			}
		}
		empty := []string{}
		for zone := range eligible {
			if len(byZone[zone]) == 0 {
				empty = append(empty, zone)
			}
		}
		if len(empty) == 0 {
			continue
		}
		crowdedZone := ""
		for zone, ids := range byZone {
			if len(ids) < 2 {
				continue
			}
			if crowdedZone == "" || len(ids) > len(byZone[crowdedZone]) ||
				(len(ids) == len(byZone[crowdedZone]) && zone > crowdedZone) {
				crowdedZone = zone
			}
		}
		if crowdedZone == "" {
			continue
		}
		ids := byZone[crowdedZone]
		sort.Strings(ids)
		sort.Strings(empty)
		return ids[len(ids)-1], class, crowdedZone, empty[0]
	}
	return "", "", "", ""
}

// rebalanceHold says what keeps a rebalance from being asked for now, or
// nothing when the database is settled enough for one replacement.
func (r *ControlPlaneReconciler) rebalanceHold(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
	cluster *unstructured.Unstructured, groups []fdbRebalanceGroup,
) (string, error) {
	if held := cp.Status.ActiveOpsRef; held != "" {
		return fmt.Sprintf("operation %s holds the control plane", held), nil
	}
	var ops simplyblockv1alpha2.ControlPlaneOpsList
	if err := r.List(ctx, &ops, client.InNamespace(cp.Namespace)); err != nil {
		return "", fmt.Errorf("list the operations in %s: %w", cp.Namespace, err)
	}
	for i := range ops.Items {
		op := &ops.Items[i]
		if op.Spec.ControlPlaneRef == cp.Name && !terminalOps(op.Status.Phase) {
			return fmt.Sprintf("operation %s is in progress", op.Name), nil
		}
	}

	reconciled, found, _ := unstructured.NestedInt64(cluster.Object, "status", "generations", "reconciled")
	if !found || reconciled != cluster.GetGeneration() {
		return fmt.Sprintf("the FoundationDB operator has not reconciled generation %d of the cluster yet",
			cluster.GetGeneration()), nil
	}
	if full, _, _ := unstructured.NestedBool(cluster.Object, "status", "health", "fullReplication"); !full {
		return "the database is not fully replicated", nil
	}

	present := map[string]bool{}
	var troubled []string
	for _, g := range groups {
		present[g.id] = !g.removing
		if g.removing {
			return fmt.Sprintf("process group %s is being removed", g.id), nil
		}
		if g.troubled {
			troubled = append(troubled, g.id)
		}
	}
	pending, _, _ := unstructured.NestedStringSlice(cluster.Object, "spec", "processGroupsToRemove")
	for _, id := range pending {
		if present[id] {
			return fmt.Sprintf("process group %s is waiting to be removed", id), nil
		}
	}
	if len(troubled) > 0 {
		sort.Strings(troubled)
		return fmt.Sprintf("process groups %s report a condition", strings.Join(troubled, ", ")), nil
	}
	return "", nil
}
