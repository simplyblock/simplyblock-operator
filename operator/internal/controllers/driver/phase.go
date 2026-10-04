// What the deployment's two plugins are doing, read off their workloads.
//
// The two fail differently and the phase has to say which: a node plugin down
// strands one worker's volumes while every other worker keeps attaching, and a
// controller plugin down stops provisioning for the whole cluster while every
// existing attachment survives. Collapsing those into one unhealthy phase hides
// the difference between a partial failure and an outage.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §4.2.

package driver

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// health is everything §4.2 derives, computed together because the phase is
// meaningless without the counts that explain it.
type health struct {
	phase           simplyblockv1alpha2.SimplyblockDriverPhase
	nodesReady      int32
	nodesTotal      int32
	controllerReady bool
	message         string
}

// derive reads the phase off the two workloads and the registration. A nil
// workload is one the apply has not produced yet, which is Installing rather
// than a failure.
func derive(node *appsv1.DaemonSet, controller *appsv1.StatefulSet, registered bool) health {
	h := health{}
	if node != nil {
		h.nodesReady = node.Status.NumberReady
		h.nodesTotal = node.Status.DesiredNumberScheduled
	}
	if controller != nil {
		h.controllerReady = controller.Status.ReadyReplicas > 0
	}

	switch {
	case node == nil || controller == nil || !registered:
		h.phase = simplyblockv1alpha2.SimplyblockDriverPhaseInstalling
		h.message = "the deployment's objects are still being applied"

	// The controller plugin decides first, and on its own. It is the plugin
	// that creates and deletes volumes, so provisioning has stopped whatever
	// the node plugins are doing.
	case !h.controllerReady:
		h.phase = simplyblockv1alpha2.SimplyblockDriverPhaseUnavailable
		h.message = "the controller plugin is not running, so no volume can be provisioned; " +
			"existing attachments are unaffected"

	case h.nodesReady < h.nodesTotal:
		h.phase = simplyblockv1alpha2.SimplyblockDriverPhaseDegraded
		h.message = fmt.Sprintf(
			"%d of %d node plugins are ready; the workers without one cannot attach a volume, "+
				"and provisioning is unaffected",
			h.nodesReady, h.nodesTotal)

	// Nothing is expected and nothing is missing. A selector that matches no
	// worker is a configuration somebody wrote, so it is said rather than
	// treated as a fault.
	case h.nodesTotal == 0:
		h.phase = simplyblockv1alpha2.SimplyblockDriverPhaseReady
		h.message = "no worker matches the node selector, so no node plugin is scheduled " +
			"and no workload on this cluster can attach a volume"

	default:
		h.phase = simplyblockv1alpha2.SimplyblockDriverPhaseReady
		h.message = fmt.Sprintf("%d of %d node plugins ready, and the controller plugin is serving",
			h.nodesReady, h.nodesTotal)
	}

	return h
}

// eventFor is the event a phase owes when the deployment arrives at it. The
// events mark transitions rather than states, so a deployment that stays
// Degraded says so once instead of every resync.
func eventFor(h health) (eventType, reason string, ok bool) {
	switch h.phase {
	case simplyblockv1alpha2.SimplyblockDriverPhaseReady:
		if h.nodesTotal == 0 {
			return "Normal", reasonNoMatchingWorkers, true
		}
		return "Normal", reasonDriverReady, true
	case simplyblockv1alpha2.SimplyblockDriverPhaseDegraded:
		return "Warning", reasonDriverDegraded, true
	case simplyblockv1alpha2.SimplyblockDriverPhaseUnavailable:
		return "Warning", reasonDriverUnavailable, true
	default:
		return "", "", false
	}
}
