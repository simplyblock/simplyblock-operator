// A removal call the control plane refuses for now rather than for good.
//
// prepare-removal and the node DELETE run the same admission, and some of its
// refusals pass by themselves: a cluster still rebalancing, an active task on
// the node, a peer that is down. Failing the operation on one of those, with
// the node already shut down, leaves the node offline for a condition that
// clears minutes later. The operation waits instead, and the step's deadline
// bounds the wait.
//
// design-storagenode.md §8.2 and §8.3.

package node

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// bareRemovalRefusal is the control plane's answer to a refused node DELETE,
// which carries no reason.
var bareRemovalRefusal = &ControlPlaneError{
	Status: http.StatusBadRequest, Body: `{"detail":"Failed to remove storage node"}`,
}

// admissionRefusal is the answer the admission gives with its reason, the way
// prepare-removal answers and the DELETE answers once it carries one.
func admissionRefusal(reason string) *ControlPlaneError {
	return &ControlPlaneError{
		Status: http.StatusBadRequest,
		Body: `{"error":"Preconditions are not met","detail":"Can not remove node ` + opsNodeID +
			`: ` + reason + `"}`,
	}
}

// inCluster sets what the cluster stream reports about the operation's cluster.
func inCluster(r *StorageNodeOpsReconciler, status string, rebalancing bool) {
	r.Clusters = &deliveredCluster{synced: true, reading: subscriptions.ClusterDTO{
		ID: opsClusterID, Status: status, Rebalancing: rebalancing,
	}}
}

// deferred asserts the step waited on the refusal rather than failing on it.
func deferred(t *testing.T, done bool, err error) {
	t.Helper()
	var fatal *terminalStepError
	if errors.As(err, &fatal) {
		t.Fatalf("err = %v, want the operation waiting on a refusal that passes by itself", err)
	}
	var blocked *blockedStepError
	if !errors.As(err, &blocked) || blocked.reason != RemovalDeferred {
		t.Errorf("err = %v, want the step held with %s", err, RemovalDeferred)
	}
	if done {
		t.Error("the step finished on a refusal")
	}
}

// Regression: 2026-10-05-removal-refusal-read-as-final — the DELETE landed
// while the cluster was still rebalancing after the drain's own migrations, the
// control plane refused it with a bare 400, and the operation failed with every
// volume already moved and the node shut down.
func TestADeleteRefusedWhileTheClusterIsBusyWaits(t *testing.T) {
	for name, cluster := range map[string]struct {
		status      string
		rebalancing bool
	}{
		"rebalancing": {utils.ClusterStatusActive, true},
		"degraded":    {clusterStatusDegraded, false},
	} {
		t.Run(name, func(t *testing.T) {
			api := aControlPlane().reporting(nodeStatusMigratingLvols).refusing("RemoveNode", bareRemovalRefusal)
			r, _ := aDraining(t, api, &scriptedMover{})
			inCluster(r, cluster.status, cluster.rebalancing)

			done, err := performing(t, r, aDrain(), stepRemoving)
			deferred(t, done, err)
		})
	}
}

// A bare refusal with the cluster active and settled is the admission saying
// the cluster cannot afford to lose the node, which no wait changes.
func TestABareDeleteRefusalOnASettledClusterIsFinal(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusMigratingLvols).refusing("RemoveNode", bareRemovalRefusal)
	r, _ := aDraining(t, api, &scriptedMover{})
	inCluster(r, utils.ClusterStatusActive, false)

	_, err := performing(t, r, aDrain(), stepRemoving)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for a refusal on a settled cluster", err)
	}
}

// Regression: 2026-10-05-removal-refusal-read-as-final — a refusal whose reason
// passes by itself is waited on whatever the cluster's status says: an active
// task on the node ends, and a peer that is down comes back.
func TestADeleteRefusedForAReasonThatPassesWaits(t *testing.T) {
	for name, reason := range map[string]string{
		"an active task": "2 active task(s) on the node; use force_remove",
		"a peer down":    "FTT=1 (npcs=1): cannot remove node, cluster already has 1 not-online node(s)",
	} {
		t.Run(name, func(t *testing.T) {
			api := aControlPlane().reporting(nodeStatusMigratingLvols).
				refusing("RemoveNode", admissionRefusal(reason))
			r, _ := aDraining(t, api, &scriptedMover{})
			inCluster(r, utils.ClusterStatusActive, false)

			done, err := performing(t, r, aDrain(), stepRemoving)
			deferred(t, done, err)
			if err != nil && !strings.Contains(err.Error(), reason) {
				t.Errorf("err = %q, want the control plane's reason in it", err)
			}
		})
	}
}

// A refusal whose reason does not pass, such as a failure-domain balance the
// removal would break, is final even while the cluster happens to be busy.
func TestARefusalForAReasonThatDoesNotPassIsFinal(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusMigratingLvols).refusing("RemoveNode",
		admissionRefusal("removal would leave failure domain 2 with 1 host"))
	r, _ := aDraining(t, api, &scriptedMover{})
	inCluster(r, utils.ClusterStatusActive, true)

	_, err := performing(t, r, aDrain(), stepRemoving)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for a refusal that no wait changes", err)
	}
}

// Regression: 2026-10-05-removal-refusal-read-as-final — prepare-removal runs
// the same admission on the offline node, and a refusal while the cluster is
// still rebalancing failed the operation with the node left offline.
func TestAnAdmissionRefusedForAReasonThatPassesWaits(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusOffline).refusing("PrepareRemoval",
		admissionRefusal("Cluster is rebalancing with 4 active nodes (1 already not online, need >3 "+
			"for ndcs=2, npcs=1). Wait for rebalancing to complete before removing a node."))
	r, _ := aDraining(t, api, &scriptedMover{})

	done, err := performing(t, r, aDrain(), stepMigratingDevices)
	deferred(t, done, err)
}
