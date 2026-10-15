// Tests for the event-driven metrics: the provisioning duration a bucket's
// own trip to Ready observes, and the mount-delivery call counter
// reconcileMountDelivery publishes. The phase gauge is collector_test.go's
// own scope, since it is collected on a timer rather than observed inline.

package objectstore

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// histogramSampleCount reads how many observations the child under these
// label values carries, the one thing testutil.ToFloat64 cannot read off a
// histogram (which has no single value).
func histogramSampleCount(vec *prometheus.HistogramVec, labelValues ...string) uint64 {
	var m dto.Metric
	if err := vec.WithLabelValues(labelValues...).(prometheus.Metric).Write(&m); err != nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

// I-13: a bucket reaching Ready observes exactly one provisioning-duration
// sample and exactly one successful deliver call, under its own
// ObjectStore's and node's labels.
func TestReadyBucketObservesProvisioningDurationAndDeliveryCall(t *testing.T) {
	readyBucketWithGatewayPod(t, "metrics-store", "metrics-bucket", "node-a", "node-a")

	if got := testutil.ToFloat64(mountDeliveryCallsTotal.WithLabelValues("metrics-store", "node-a", "deliver", "success")); got != 1 {
		t.Errorf("deliver/success calls for node-a = %v, want 1", got)
	}
	if got := histogramSampleCount(bucketProvisioningSeconds, "metrics-store"); got != 1 {
		t.Errorf("provisioning-duration samples for metrics-store = %d, want 1", got)
	}
}
