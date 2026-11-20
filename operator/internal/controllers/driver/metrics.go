// The gauges the operator publishes about the CSI driver it deploys.
//
// All four are gauges and none of them counts an event, because a driver has no
// operations: it is deployed, and what matters is whether it is up. The question
// a dashboard asks is whether provisioning works right now, which is the
// controller plugin, and whether every worker can attach, which is the two node
// counts read as a ratio.
//
// Neither node count means anything alone. Three ready plugins is healthy on a
// three-worker cluster and an outage on a thirty-worker one, so the pair is
// published from the same pass or not at all.
//
// design-simplyblockdriver.md §6.2 is the specification, and §7.12 of
// design-crd-model.md is the naming rule the suffixes follow.

package driver

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// driverVersionInfo carries the reported version in a label and is 1 for it,
	// which is the shape §7.12 gives an `info` gauge. Beside the control plane's
	// own version gauge, the pair is the skew alert: a driver and a control plane
	// that disagree fail in the data path at attach time, on a workload's pod.
	//
	// TODO(simplyblockdriver): it is 0 today, because status.version is written
	// by nothing. The two missing pieces are recorded on the TODO in
	// simplyblockdriver_controller.go beside the field: GET /_meta/version on the
	// management API, and the release document that says which control planes a
	// driver works against. Zero is the honest reading until then — an info gauge
	// is 1 for a fact it carries, and there is no fact yet — and the series
	// becomes correct on the day the field is filled in, with no change here.
	driverVersionInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_simplyblockdriver_version_info",
			Help: "1 for the version the deployed driver reports, and 0 while it reports none.",
		},
		[]string{"namespace", "version"},
	)

	driverNodesReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_simplyblockdriver_nodes_ready_count",
			Help: "Workers running a ready node plugin.",
		},
		[]string{"namespace"},
	)

	driverNodesExpected = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_simplyblockdriver_nodes_expected_count",
			Help: "Workers expected to run one, so the ratio against nodes_ready_count is the alert.",
		},
		[]string{"namespace"},
	)

	driverControllerReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_simplyblockdriver_controller_ready_state",
			Help: "1 while the controller plugin serves. Provisioning stops when it is zero.",
		},
		[]string{"namespace"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		driverVersionInfo,
		driverNodesReady,
		driverNodesExpected,
		driverControllerReady,
	)
}

// observeHealth publishes one reading of a deployment.
//
// The version series is replaced rather than added to. An info gauge says which
// fact is current by carrying it in a label, so a driver that was upgraded would
// otherwise leave the version it used to run sitting at 1 beside the one it
// runs now, and a dashboard reading either would be reading a claim about the
// present.
func observeHealth(namespace, version string, h health) {
	driverVersionInfo.DeletePartialMatch(prometheus.Labels{"namespace": namespace})
	reported := 0.0
	if version != "" {
		reported = 1
	}
	driverVersionInfo.WithLabelValues(namespace, version).Set(reported)

	driverNodesReady.WithLabelValues(namespace).Set(float64(h.nodesReady))
	driverNodesExpected.WithLabelValues(namespace).Set(float64(h.nodesTotal))

	serving := 0.0
	if h.controllerReady {
		serving = 1
	}
	driverControllerReady.WithLabelValues(namespace).Set(serving)
}
