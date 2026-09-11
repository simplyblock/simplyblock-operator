// The gauges the operator publishes about storage devices.
//
// They are declared apart from the collector that fills them for the reason
// every metrics file in this package is: a gauge is process-global state
// registered once at init, and a collector is a Runnable that may be constructed
// more than once. Keeping them separate is also what lets a test reset the
// series without reaching into the collector.
//
// A device is labeled by its object name rather than by the control plane's
// device id. These gauges exist so that a Kubernetes-side dashboard can name
// what it is showing, and the used size they carry is published here rather
// than joined from the control plane's own exporter, so nothing needs the
// backend id to line the two up.

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// deviceLabelNames are the labels every per-device gauge carries.
var deviceLabelNames = []string{"cluster", "node", "device"}

var (
	deviceCapacityBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagedevice_capacity_bytes",
			Help: "Size of the storage device, as the control plane reports it.",
		},
		deviceLabelNames,
	)

	deviceUsedBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagedevice_used_bytes",
			Help: "Space the storage device holds. Absent while no capacity sample is available.",
		},
		deviceLabelNames,
	)

	// One series per phase rather than one carrying a phase number: a dashboard
	// graphing "how many devices are failed" should not have to know which
	// integer means failed, and a phase added later would renumber the rest.
	devicePhaseState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagedevice_phase_state",
			Help: "1 for the phase the storage device is in, 0 for every other phase.",
		},
		append(append([]string{}, deviceLabelNames...), "phase"),
	)

	nodeDeviceCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagenode_devices_count",
			Help: "Devices the storage node has, so the ratio matches the node's own summary.",
		},
		[]string{"cluster", "node"},
	)

	// The redundancy signal: erasure coding survives a bounded number of
	// simultaneous losses, and this is the input to whether the next one is
	// survivable.
	nodeDeviceFailedCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagenode_devices_failed_count",
			Help: "Failed devices on the storage node.",
		},
		[]string{"cluster", "node"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		deviceCapacityBytes,
		deviceUsedBytes,
		devicePhaseState,
		nodeDeviceCount,
		nodeDeviceFailedCount,
	)
}

// resetDeviceMetrics drops every series. The collector calls it at the top of
// each pass, which is what makes a device that went away stop being reported: a
// gauge nobody updates any more keeps its last value forever, and a failed drive
// that a dashboard says is fine is worse than no dashboard.
func resetDeviceMetrics() {
	deviceCapacityBytes.Reset()
	deviceUsedBytes.Reset()
	devicePhaseState.Reset()
	nodeDeviceCount.Reset()
	nodeDeviceFailedCount.Reset()
}
