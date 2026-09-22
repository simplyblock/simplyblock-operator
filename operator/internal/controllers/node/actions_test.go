// The four actions that are one call and one wait.
//
// Shutdown, Restart, Suspend, and Resume each issue a single request and then
// watch for the state it produces. Both halves are written against current state
// rather than against a transition: the call is skipped when the node is already
// at or past where that call would put it, and the wait is a predicate over what
// the control plane reports now.
//
// That is what makes a step recorded without its side effect having fired safe to
// re-enter, which is the whole reason this operator keeps no `triggered` flag
// beside the step. A suspend that was issued and whose response was lost is
// re-entered, reads a suspended node, and advances without a second suspend.
//
// design-storagenode.md §7.2 and §7.3.

package node

import (
	"context"
	"errors"
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// requested runs the Requesting step of one action against a node the control
// plane reports in the given state, and reports what was asked of it.
func requested(
	t *testing.T,
	action simplyblockv1alpha2.StorageNodeOpsAction,
	status string,
) (*scriptedControlPlane, bool) {
	t.Helper()
	api := aControlPlane().reporting(status)
	r, _ := anOpsWorld(t, api)

	done, err := r.perform(context.Background(),
		anOperation("an-operation", action), stepRequesting)
	if err != nil {
		t.Fatalf("the %s request: %v", action, err)
	}
	return api, done
}

// A node that is already where the call would put it receives no call, and the
// step is finished. One call per action, and the state each skips on is the state
// that call produces.
func TestACallIsSkippedWhenTheNodeIsAlreadyThere(t *testing.T) {
	cases := []struct {
		action simplyblockv1alpha2.StorageNodeOpsAction
		status string
		call   string
	}{
		{simplyblockv1alpha2.StorageNodeOpsActionShutdown, nodeStatusOffline, "ShutdownNode"},
		{simplyblockv1alpha2.StorageNodeOpsActionSuspend, nodeStatusSuspended, "Suspend"},
		{simplyblockv1alpha2.StorageNodeOpsActionResume, nodeStatusOnline, "Resume"},
	}
	for _, c := range cases {
		t.Run(string(c.action), func(t *testing.T) {
			api, done := requested(t, c.action, c.status)

			if asked := api.asked(c.call); asked != 0 {
				t.Errorf("%s was issued %d time(s) against a node already %s",
					c.call, asked, c.status)
			}
			if !done {
				t.Error("the step did not finish against a node already where it was going")
			}
		})
	}
}

// A node that is not there yet receives the call.
func TestACallIsIssuedWhenTheNodeIsNotThereYet(t *testing.T) {
	cases := []struct {
		action simplyblockv1alpha2.StorageNodeOpsAction
		status string
		call   string
	}{
		{simplyblockv1alpha2.StorageNodeOpsActionShutdown, nodeStatusOnline, "ShutdownNode"},
		{simplyblockv1alpha2.StorageNodeOpsActionSuspend, nodeStatusOnline, "Suspend"},
		{simplyblockv1alpha2.StorageNodeOpsActionResume, nodeStatusSuspended, "Resume"},
	}
	for _, c := range cases {
		t.Run(string(c.action), func(t *testing.T) {
			api, _ := requested(t, c.action, c.status)

			if asked := api.asked(c.call); asked != 1 {
				t.Errorf("%s was issued %d time(s), want once", c.call, asked)
			}
		})
	}
}

// A restart has no state of its own to skip on: a node is online before one and
// online after it. What guards the second call is the persisted step and the wait
// that follows, which does not finish until the node is back.
func TestARestartIsIssuedAgainstAnOnlineNode(t *testing.T) {
	api, _ := requested(t, simplyblockv1alpha2.StorageNodeOpsActionRestart, nodeStatusOnline)

	if asked := api.asked("RestartNode"); asked != 1 {
		t.Errorf("RestartNode was issued %d time(s), want once", asked)
	}
}

// The two modifiers travel only when the operation states them, because the
// control plane defaults them itself and not sending one is not the same as
// sending false.
func TestOnlyTheFlagsTheOperationStatesAreSent(t *testing.T) {
	api := aControlPlane()
	r, _ := anOpsWorld(t, api)

	ops := anOperation("a-restart", simplyblockv1alpha2.StorageNodeOpsActionRestart)
	if _, err := r.perform(context.Background(), ops, stepRequesting); err != nil {
		t.Fatalf("the restart request: %v", err)
	}
	if len(api.restarts) != 1 {
		t.Fatalf("%d restarts were issued, want one", len(api.restarts))
	}
	if api.restarts[0].Force || api.restarts[0].ReattachVolume {
		t.Errorf("params = %+v, want both flags left to the control plane's own defaults",
			api.restarts[0])
	}

	stated := aControlPlane()
	r, _ = anOpsWorld(t, stated)
	ops = anOperation("a-forced-restart", simplyblockv1alpha2.StorageNodeOpsActionRestart)
	ops.Spec.Force = ptr.To(true)
	ops.Spec.ReattachVolume = ptr.To(true)
	if _, err := r.perform(context.Background(), ops, stepRequesting); err != nil {
		t.Fatalf("the forced restart request: %v", err)
	}
	if !stated.restarts[0].Force || !stated.restarts[0].ReattachVolume {
		t.Errorf("params = %+v, want what the operation stated", stated.restarts[0])
	}
}

// The wait is over when the node reports the state the action was for, and not
// before.
func TestTheWaitIsOverWhenTheNodeReportsWhatTheActionWasFor(t *testing.T) {
	cases := []struct {
		action simplyblockv1alpha2.StorageNodeOpsAction
		wanted string
		other  string
	}{
		{simplyblockv1alpha2.StorageNodeOpsActionShutdown, nodeStatusOffline, nodeStatusOnline},
		{simplyblockv1alpha2.StorageNodeOpsActionRestart, nodeStatusOnline, nodeStatusInRestart},
		{simplyblockv1alpha2.StorageNodeOpsActionSuspend, nodeStatusSuspended, nodeStatusOnline},
		{simplyblockv1alpha2.StorageNodeOpsActionResume, nodeStatusOnline, nodeStatusSuspended},
	}
	for _, c := range cases {
		t.Run(string(c.action), func(t *testing.T) {
			r, _ := anOpsWorld(t, aControlPlane().reporting(c.other))
			ops := anOperation("an-operation", c.action)

			done, err := r.perform(context.Background(), ops, stepAwaiting)
			if err != nil {
				t.Fatalf("the wait: %v", err)
			}
			if done {
				t.Errorf("the wait finished against a node reporting %s", c.other)
			}

			r, _ = anOpsWorld(t, aControlPlane().reporting(c.wanted))
			done, err = r.perform(context.Background(), ops, stepAwaiting)
			if err != nil {
				t.Fatalf("the wait: %v", err)
			}
			if !done {
				t.Errorf("the wait did not finish against a node reporting %s", c.wanted)
			}
		})
	}
}

// An action that runs a graph of its own does not issue a single request, and a
// step reached under one that does is a hand-edited object or a downgrade.
// Neither resolves by reconciling again, so both are terminal.
func TestAStepThatBelongsToNoActionEndsTheOperation(t *testing.T) {
	r, _ := anOpsWorld(t, aControlPlane())

	_, err := r.perform(context.Background(),
		anOperation("a-drain", simplyblockv1alpha2.StorageNodeOpsActionRemove), stepRequesting)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for an action that issues no single request", err)
	}

	_, err = r.perform(context.Background(),
		anOperation("an-operation", simplyblockv1alpha2.StorageNodeOpsActionSuspend), step("Nowhere"))
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for a step no action declares", err)
	}
}

// A node the object has no UUID for has not been provisioned, which no number of
// passes will change.
func TestAnUnprovisionedNodeEndsTheOperation(t *testing.T) {
	node := anOpsNode()
	node.Status.UUID = ""
	r, apiClient := anOpsWorld(t, aControlPlane())
	if err := apiClient.Delete(context.Background(), anOpsNode()); err != nil {
		t.Fatalf("clearing the provisioned node: %v", err)
	}
	if err := apiClient.Create(context.Background(), node); err != nil {
		t.Fatalf("seeding the unprovisioned node: %v", err)
	}

	_, err := r.perform(context.Background(),
		anOperation("an-operation", simplyblockv1alpha2.StorageNodeOpsActionSuspend), stepRequesting)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for a node with no backend behind it", err)
	}
}
