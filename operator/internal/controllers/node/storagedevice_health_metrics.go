// The gauges the health monitor publishes about storage devices.
//
// They are declared apart from the monitor for the reason storagedevice_metrics.go
// gives: a gauge is process-global state registered once, and a monitor is a
// Runnable that a test constructs many times. They carry the same labels as the
// collector's device gauges, so a dashboard joins the two on them.
//
// The verdicts are here as well as on the object because an Alertmanager rule
// is how a Kubernetes-side user receives an alert, and a rule needs a series.

package node

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

var (
	devicePerformanceState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagedevice_performance_state",
			Help: "1 for the performance verdict the storage device carries (Ok, Slow, Unknown), 0 for the others.",
		},
		append(append([]string{}, deviceLabelNames...), "verdict"),
	)

	deviceSMARTState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagedevice_smart_state",
			Help: "1 for the SMART verdict the storage device carries (Ok, Warning, Failing, Unsupported, Unknown), 0 for the others.",
		},
		append(append([]string{}, deviceLabelNames...), "verdict"),
	)

	deviceLatencyRatio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagedevice_relative_latency_count",
			Help: "Mean IO latency of the storage device over its fleet's median, a dimensionless ratio (1 is the median), from the last assessment.",
		},
		deviceLabelNames,
	)

	deviceIOPSShareRatio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagedevice_relative_iops_share_count",
			Help: "IOPS of the storage device over the share its weight entitles it to, a dimensionless ratio (1 is its full share), from the last assessment.",
		},
		deviceLabelNames,
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		devicePerformanceState,
		deviceSMARTState,
		deviceLatencyRatio,
		deviceIOPSShareRatio,
	)
}

// resetDeviceHealthMetrics drops every series, so a device that went away stops
// being reported. The monitor calls it at the top of each pass.
func resetDeviceHealthMetrics() {
	devicePerformanceState.Reset()
	deviceSMARTState.Reset()
	deviceLatencyRatio.Reset()
	deviceIOPSShareRatio.Reset()
}

// publishDeviceHealth writes one device's verdicts as one series per verdict.
func publishDeviceHealth(device *simplyblockv1alpha2.StorageDevice) {
	health := device.Status.Health
	if health == nil {
		return
	}
	labels := []string{device.Labels[simplyblockv1alpha2.DeviceLabelCluster], device.Spec.NodeRef, device.Name}
	if p := health.Performance; p != nil && p.Verdict != "" {
		for _, v := range []simplyblockv1alpha2.DevicePerformanceVerdict{
			simplyblockv1alpha2.DevicePerformanceOk,
			simplyblockv1alpha2.DevicePerformanceSlow,
			simplyblockv1alpha2.DevicePerformanceUnknown,
		} {
			devicePerformanceState.WithLabelValues(append(labels, string(v))...).Set(indicator(p.Verdict == v))
		}
	}
	if s := health.SMART; s != nil && s.Verdict != "" {
		for _, v := range []simplyblockv1alpha2.DeviceSMARTVerdict{
			simplyblockv1alpha2.DeviceSMARTOk,
			simplyblockv1alpha2.DeviceSMARTWarning,
			simplyblockv1alpha2.DeviceSMARTFailing,
			simplyblockv1alpha2.DeviceSMARTUnsupported,
			simplyblockv1alpha2.DeviceSMARTUnknown,
		} {
			deviceSMARTState.WithLabelValues(append(labels, string(v))...).Set(indicator(s.Verdict == v))
		}
	}
}

// publishDeviceRatios writes the measurements of one assessment.
func publishDeviceRatios(device *simplyblockv1alpha2.StorageDevice, a fleetAssessment) {
	if !a.measured {
		return
	}
	labels := []string{device.Labels[simplyblockv1alpha2.DeviceLabelCluster], device.Spec.NodeRef, device.Name}
	deviceLatencyRatio.WithLabelValues(labels...).Set(a.LatencyRatio / 100)
	deviceIOPSShareRatio.WithLabelValues(labels...).Set(a.IOPSRatio / 100)
}

func indicator(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
