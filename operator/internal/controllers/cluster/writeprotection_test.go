// The SwitchWriteProtection action: it moves an upgraded cluster's distribs from
// v1 to v2 write protection, once every node is online, and finishes when the
// control plane reports v2 (design-release-upgrade.md §4.4).

package cluster

import (
	"net/http"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	atlascp "github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// writeProtectionWorld is a cluster on the given generation whose nodes report
// the given statuses, with a switch that moves the streamed record to v2.
func writeProtectionWorld(
	t *testing.T, v2 *bool, statuses ...string,
) (*StorageClusterOpsReconciler, *fakeControlPlane, *recorder, *fakeClusterCache) {
	t.Helper()
	cache := streamedCluster(func(dto *subscriptions.ClusterDTO) { dto.WriteProtectionV2 = v2 })
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		storageNodes: func(string) ([]utils.NodeStatusResponse, error) {
			out := make([]utils.NodeStatusResponse, 0, len(statuses))
			for i, status := range statuses {
				out = append(out, utils.NodeStatusResponse{UUID: []string{nodeA, nodeB}[i], Status: status})
			}
			return out, nil
		},
		switchWriteProtection: func(id string) error {
			dto := cache.clusters[id]
			dto.WriteProtectionV2 = ptr.To(true)
			cache.clusters[id] = dto
			return nil
		},
	}
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec, newTestCluster(),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionSwitchWriteProtection))
	r.Clusters = cache
	return r, api, rec, cache
}

func TestTheSwitchIsMadeOnceAndFinishesOnV2(t *testing.T) {
	r, api, _, _ := writeProtectionWorld(t, ptr.To(false), utils.NodeStatusOnline, utils.NodeStatusOnline)

	ops, cluster := reconcileOps(t, r, 10)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (step %q, message %q)",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	if api.switchWriteProtectionCalls != 1 {
		t.Errorf("the switch was made %d times, want once", api.switchWriteProtectionCalls)
	}
	if cluster.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want the lock released", cluster.Status.ActiveOpsRef)
	}
}

func TestAClusterAlreadyOnV2IsNotSwitchedAgain(t *testing.T) {
	r, api, _, _ := writeProtectionWorld(t, ptr.To(true), utils.NodeStatusOnline)

	ops, _ := reconcileOps(t, r, 10)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", ops.Status.Phase)
	}
	if api.switchWriteProtectionCalls != 0 {
		t.Errorf("the switch was made %d times, want none", api.switchWriteProtectionCalls)
	}
	if !strings.Contains(ops.Status.Message, "v2 write protection") {
		t.Errorf("message = %q, want it to say the cluster runs v2", ops.Status.Message)
	}
}

func TestTheSwitchWaitsForEveryNodeToBeOnline(t *testing.T) {
	r, api, _, _ := writeProtectionWorld(t, ptr.To(false), utils.NodeStatusOnline, utils.NodeStatusOffline)

	ops, _ := reconcileOps(t, r, 10)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseRunning {
		t.Fatalf("phase = %q, want Running while a node is offline", ops.Status.Phase)
	}
	if ops.Status.Step.State != string(stepCheckingPeers) {
		t.Errorf("step = %q, want it holding in CheckingPeers", ops.Status.Step.State)
	}
	if api.switchWriteProtectionCalls != 0 {
		t.Errorf("the switch was made %d times with a node offline, want none", api.switchWriteProtectionCalls)
	}
}

func TestAControlPlaneThatDoesNotReportWriteProtectionFailsTheSwitch(t *testing.T) {
	r, api, _, _ := writeProtectionWorld(t, nil, utils.NodeStatusOnline)

	ops, _ := reconcileOps(t, r, 10)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if api.switchWriteProtectionCalls != 0 {
		t.Errorf("the switch was made %d times, want none", api.switchWriteProtectionCalls)
	}
}

func TestARefusedSwitchIsNotRepeatedWithinItsClaim(t *testing.T) {
	r, api, _, _ := writeProtectionWorld(t, ptr.To(false), utils.NodeStatusOnline)
	api.switchWriteProtection = func(string) error {
		return &atlascp.StatusError{Op: "switch", StatusCode: http.StatusConflict, Body: "stays on v1"}
	}

	ops, _ := reconcileOps(t, r, 10)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseRunning {
		t.Fatalf("phase = %q, want Running: a refused switch is asked again, not given up on", ops.Status.Phase)
	}
	if api.switchWriteProtectionCalls != 1 {
		t.Errorf("the switch was made %d times within one claim, want once", api.switchWriteProtectionCalls)
	}
}
