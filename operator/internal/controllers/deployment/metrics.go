// The metrics this package publishes about deployment documents and the
// discovery runs that write them.
//
// Both families are new, because neither kind existed before the redesign and
// nothing measured what a deployment costs. Two of them answer questions nothing
// else here can:
//
//   - validation_failures_total by reason says whether the approval gate is
//     working as review or as an obstacle. A deployment where every draft fails
//     on DeviceNotFound is a discovery bug rather than a careful reviewer, and
//     the reason label is what separates the two.
//   - approval_rejections_total is the pair to read it against. The two count
//     the same mistakes at different moments, so a rejection that draft
//     validation never reported first is a gap in the validation: the reviewer
//     should have been told before they wrote the approval.
//
// A rejected approval has no object to record an event against, since an
// admission rejection fails the request. These counters are the only signal that
// path has, which is why the webhook reaches into this package to raise one.
//
// design-clusterdeploymentconfig.md §9.2 is the specification, and §7.12 of
// design-crd-model.md is the naming rule the suffixes follow.

package deployment

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The reasons only the webhook produces. The four that a draft's validation also
// finds are the event reasons in events.go, so the two counters share a
// vocabulary and can be read against each other; these three have no event
// because there is no object to raise one on.
const (
	// NoClusterNamed is a document that names neither a cluster to create nor
	// one to grow, so it describes no deployment at all.
	NoClusterNamed = "NoClusterNamed"

	// ApprovalWithdrawn is an edit clearing spec.approved, which does not
	// un-expand what the document already built.
	ApprovalWithdrawn = "ApprovalWithdrawn"

	// SpecImmutable is an edit to the spec of a document that is already the
	// record of what was deployed.
	SpecImmutable = "SpecImmutable"
)

// expansionBuckets span what an expansion actually takes. Creating the objects
// is seconds, and waiting for the control plane to report the cluster is
// minutes, so a linear set would put every interesting deployment in one bucket.
var expansionBuckets = prometheus.ExponentialBuckets(1, 3, 10)

var (
	configPhaseState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_clusterdeploymentconfig_phase_state",
			Help: "1 for the document's current phase and 0 for the rest, so one stuck in Draft is visible.",
		},
		[]string{"namespace", "name", "phase"},
	)

	expansionDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_clusterdeploymentconfig_expansion_duration_seconds",
			Help:    "How long an expansion took, from the approval that started it to Expanded.",
			Buckets: expansionBuckets,
		},
		[]string{"namespace"},
	)

	nodesCreatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_clusterdeploymentconfig_nodes_created_total",
			Help: "StorageNode objects an expansion created, which is the fleet's growth over time.",
		},
		[]string{"namespace"},
	)

	validationFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_clusterdeploymentconfig_validation_failures_total",
			Help: "Draft validation failures by reason, which is what a reviewer keeps hitting.",
		},
		[]string{"namespace", "reason"},
	)

	approvalRejectionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_clusterdeploymentconfig_approval_rejections_total",
			Help: "Approving edits admission rejected, by reason: the gate catching a mistake before it is immutable.",
		},
		[]string{"namespace", "reason"},
	)

	operatorOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_operator_operations_total",
			Help: "Operator operations that reached a terminal phase, by succeeded, failed, and aborted.",
		},
		[]string{"namespace", "action", "result"},
	)

	operatorOperationDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_operator_operation_duration_seconds",
			Help:    "How long an operator operation took, from its start to a terminal phase.",
			Buckets: expansionBuckets,
		},
		[]string{"namespace", "action"},
	)

	discoveryWorkersFound = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_operator_discovery_workers_found_count",
			Help: "Workers the last discovery run of this namespace found.",
		},
		[]string{"namespace"},
	)

	discoveryDevicesFound = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_operator_discovery_devices_found_count",
			Help: "Candidate devices it found, which is the number the draft is written from.",
		},
		[]string{"namespace"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		configPhaseState,
		expansionDurationSeconds,
		nodesCreatedTotal,
		validationFailuresTotal,
		approvalRejectionsTotal,
		operatorOperationsTotal,
		operatorOperationDurationSeconds,
		discoveryWorkersFound,
		discoveryDevicesFound,
	)
}

// observeConfigPhase publishes the document's phase as a gauge that is 1 for
// where it is and 0 for everywhere else, so a query for one phase answers for
// every document rather than only for the ones currently in it.
func observeConfigPhase(config *simplyblockv1alpha2.ClusterDeploymentConfig) {
	for _, phase := range []simplyblockv1alpha2.ClusterDeploymentConfigPhase{
		simplyblockv1alpha2.ClusterDeploymentConfigPhaseDraft,
		simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanding,
		simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanded,
		simplyblockv1alpha2.ClusterDeploymentConfigPhaseFailed,
	} {
		value := 0.0
		if config.Status.Phase == phase {
			value = 1
		}
		configPhaseState.
			WithLabelValues(config.Namespace, config.Name, string(phase)).
			Set(value)
	}
}

// forgetConfig drops a deleted document's series. A gauge keyed on an object's
// name outlives the object, and four series reporting the phase of a document
// nobody can look at is worse than no series at all.
func forgetConfig(config *simplyblockv1alpha2.ClusterDeploymentConfig) {
	configPhaseState.DeletePartialMatch(prometheus.Labels{
		"namespace": config.Namespace,
		"name":      config.Name,
	})
}

// countValidationFailures records one failure per distinct reason.
//
// One per reason rather than one per finding, matching the events: three groups
// naming the same missing worker is one thing wrong, and counting it three times
// would make a document with many groups look like a deployment with many
// problems.
func countValidationFailures(namespace string, findings []finding) {
	seen := map[string]struct{}{}
	for _, found := range findings {
		if _, already := seen[found.reason]; already {
			continue
		}
		seen[found.reason] = struct{}{}
		validationFailuresTotal.WithLabelValues(namespace, found.reason).Inc()
	}
}

// CountApprovalRejection records an approving edit admission refused. It is
// exported because the webhook that refuses lives next door and this is the only
// record that path leaves.
func CountApprovalRejection(namespace, reason string) {
	approvalRejectionsTotal.WithLabelValues(namespace, reason).Inc()
}

// observeExpansion records how long the expansion took.
//
// A document whose start was never recorded is not measured at all. That is a
// document the operator was upgraded underneath mid-expansion, and a duration
// measured from an instant nobody wrote down would be a number the histogram
// cannot distinguish from a real one.
func observeExpansion(config *simplyblockv1alpha2.ClusterDeploymentConfig) {
	started := config.Status.ExpansionStartedAt
	if started == nil {
		return
	}
	expansionDurationSeconds.WithLabelValues(config.Namespace).
		Observe(time.Since(started.Time).Seconds())
}

// countNodesCreated records the StorageNodes an expansion pass created. It
// counts what was actually created rather than what the document describes, so
// a pass that found every node already there adds nothing.
func countNodesCreated(namespace string, created int) {
	if created <= 0 {
		return
	}
	nodesCreatedTotal.WithLabelValues(namespace).Add(float64(created))
}

// observeRun records a discovery run's outcome and how long it took.
func observeRun(
	ops *simplyblockv1alpha2.OperatorOps, phase simplyblockv1alpha2.OperatorOpsPhase,
) {
	action := string(ops.Spec.Action)
	operatorOperationsTotal.
		WithLabelValues(ops.Namespace, action, runResultOf(phase)).Inc()
	if started := ops.Status.StartedAt; started != nil {
		operatorOperationDurationSeconds.WithLabelValues(ops.Namespace, action).
			Observe(time.Since(started.Time).Seconds())
	}
}

// runResultOf is the metric label for a terminal phase, lowercased because a
// label value is not an API enum.
func runResultOf(phase simplyblockv1alpha2.OperatorOpsPhase) string {
	switch phase {
	case simplyblockv1alpha2.OperatorOpsPhaseSucceeded:
		return "succeeded"
	case simplyblockv1alpha2.OperatorOpsPhaseAborted:
		return "aborted"
	default:
		return "failed"
	}
}

// observeWorkersFound and observeDevicesFound publish what the last run of a
// namespace concluded. They are separate calls because the two numbers are
// settled by different steps: which workers the run is about is decided in
// Inspecting and never revisited, and how many devices survived the rules is
// only known once the plan is built in Writing.
func observeWorkersFound(namespace string, workers int) {
	discoveryWorkersFound.WithLabelValues(namespace).Set(float64(workers))
}

func observeDevicesFound(namespace string, devices int) {
	discoveryDevicesFound.WithLabelValues(namespace).Set(float64(devices))
}
