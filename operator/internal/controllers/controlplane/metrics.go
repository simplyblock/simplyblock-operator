// The series the operator publishes about the control plane.
//
// simplyblock_controlplane_ready_state is the one to alert on first. Everything
// else in this API group is downstream of it, so an alert storm that starts here
// has one cause and one page.
//
// The component pair is a warning rather than a page, and the essential label is
// what lets one rule separate them: a component's ready count falling below its
// desired count while the ready state stays at one is the restart loop that has
// not become an outage yet.
//
// design-controlplane.md §9.2 is the specification. Two of its nine series are
// not here: simplyblock_controlplane_version_info waits on the /_meta/version
// read §8 records as missing, since a gauge labeled with an empty version says
// nothing, and the operation counters live beside the operation that increments
// them.

package controlplane

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// componentLabels are the labels every per-component series carries. The
// essential label is on the series rather than looked up by an alert, because an
// alerting rule cannot join against a table that lives in this binary.
var componentLabels = []string{"namespace", "component", "essential"}

var (
	// controlPlaneReadyState is the probe's own verdict, which a Degraded
	// control plane still passes. It is deliberately not the phase: the phase
	// folds in the component counts, and an alert that fired on Degraded would
	// page for a pod restarting behind a Service that is still answering.
	controlPlaneReadyState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_controlplane_ready_state",
			Help: "1 while the control plane's readiness probe passes, which a Degraded control plane still does.",
		},
		[]string{"namespace"},
	)

	// controlPlaneComponentReady and controlPlaneComponentDesired are the half
	// of the phase the probe cannot see. Neither means anything alone: the
	// ratio is the alert.
	controlPlaneComponentReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_controlplane_component_ready_count",
			Help: "How many replicas of one control-plane component are ready.",
		},
		componentLabels,
	)

	controlPlaneComponentDesired = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_controlplane_component_desired_count",
			Help: "How many replicas of one control-plane component there should be.",
		},
		componentLabels,
	)

	// controlPlaneProbeDuration is latency, which degrades before readiness
	// does: a control plane answering in four seconds is on its way to not
	// answering, and the ready state cannot show that.
	controlPlaneProbeDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_controlplane_probe_duration_seconds",
			Help:    "How long one readiness probe took.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"namespace"},
	)

	// controlPlaneProbeFailures separates the ways a probe fails, because a
	// transport error and a 503 have different causes: one is the network or
	// the pod, the other is the control plane telling you what is wrong.
	controlPlaneProbeFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_controlplane_probe_failures_total",
			Help: "Failed readiness probes, by reason.",
		},
		[]string{"namespace", "reason"},
	)

	// controlPlaneInstallStepDuration is how long each installation step took,
	// which is what says whether an install that felt slow was slow and where.
	controlPlaneInstallStepDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "simplyblock_controlplane_install_step_duration_seconds",
			Help: "How long one step of the control plane's installation took.",
			// An install step is minutes rather than milliseconds, so the
			// default buckets would put every observation in the last one.
			Buckets: []float64{10, 30, 60, 120, 300, 600, 1200, 2400, 4800},
		},
		[]string{"namespace", "step"},
	)

	// controlPlaneOperationsTotal and controlPlaneOperationDuration are the two
	// cumulative series every Ops kind in this group publishes.
	controlPlaneOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_controlplane_operations_total",
			Help: "Control-plane operations that reached a terminal phase.",
		},
		[]string{"namespace", "action", "result"},
	)

	controlPlaneOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_controlplane_operation_duration_seconds",
			Help:    "How long one control-plane operation took.",
			Buckets: []float64{10, 30, 60, 120, 300, 600, 1200, 2400, 4800},
		},
		[]string{"namespace", "action"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		controlPlaneReadyState,
		controlPlaneComponentReady,
		controlPlaneComponentDesired,
		controlPlaneProbeDuration,
		controlPlaneProbeFailures,
		controlPlaneInstallStepDuration,
		controlPlaneOperationsTotal,
		controlPlaneOperationDuration,
	)
}

// probeFailureReason classifies a failed probe into the small set the counter's
// label admits. It is derived from the message rather than from a typed error
// because the message is what the probe interface returns, and the alternative
// is an error taxonomy no caller distinguishes anywhere else.
func probeFailureReason(message string) string {
	switch {
	case containsAny(message, "context deadline exceeded", "Client.Timeout", "i/o timeout"):
		return "timeout"
	case strings.Contains(message, "status="):
		return "status"
	default:
		return "transport"
	}
}

// containsAny reports whether s holds any of the needles.
func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
