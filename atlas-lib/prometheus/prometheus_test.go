// Tests for every metric family this package reads, driven through a stub
// Prometheus API rather than an HTTP server: the seam worth testing is which
// query is issued and how its labels are grouped, and a stub records the query
// verbatim where a server would only see it after a round trip.

package prometheus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// stubAPI answers one query with a fixed vector and records what it was asked.
// Only Query is implemented. Every other method of promv1.API is a
// compile-time requirement that is never called, so the interface is embedded
// to leave them unimplemented and panic if that assumption is ever wrong.
type stubAPI struct {
	promv1.API
	vector  model.Vector
	err     error
	queries []string
}

func (s *stubAPI) Query(
	_ context.Context, query string, _ time.Time, _ ...promv1.Option,
) (model.Value, promv1.Warnings, error) {
	s.queries = append(s.queries, query)
	if s.err != nil {
		return nil, nil, s.err
	}
	return s.vector, nil, nil
}

// sample builds one series. The metric name is a label like any other, which is
// what lets a single query carry several metrics at once.
func sample(name string, labels map[string]string, value float64) *model.Sample {
	m := model.Metric{model.MetricNameLabel: model.LabelValue(name)}
	for k, v := range labels {
		m[model.LabelName(k)] = model.LabelValue(v)
	}
	return &model.Sample{Metric: m, Value: model.SampleValue(value)}
}

const (
	testCluster = "2edd9a96-cc5a-473a-9e1d-0a9cf4ef8ad2"
	testVolume  = "81a0d5c5-f8bf-4c40-9c63-215b5c414a85"
	testDevice  = "d7daf385-72ea-49bf-afca-ec01bfaf538b"
)

func volumeLabels() map[string]string {
	return map[string]string{"cluster": testCluster, "lvol": testVolume}
}

// A capacity sample is assembled from six separate metrics that arrive as six
// series of one query, which is the whole reason the family helper exists.
func TestVolumeCapacityAssemblesOneSampleFromEveryMetric(t *testing.T) {
	api := &stubAPI{vector: model.Vector{
		sample("lvol_size_total", volumeLabels(), 10737418240),
		sample("lvol_size_used", volumeLabels(), 1080033280),
		sample("lvol_size_free", volumeLabels(), 9657384960),
		sample("lvol_size_prov", volumeLabels(), 10737418240),
		sample("lvol_size_util", volumeLabels(), 10),
		sample("lvol_date", volumeLabels(), 1788384958),
	}}

	got, err := NewWithAPI(api).VolumeCapacity(context.Background(), testCluster)
	if err != nil {
		t.Fatalf("VolumeCapacity: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d volumes, want 1", len(got))
	}
	c := got[testVolume]
	if c.Total != 10737418240 || c.Used != 1080033280 || c.Free != 9657384960 {
		t.Errorf("sizes = total %d, used %d, free %d", c.Total, c.Used, c.Free)
	}
	if c.Provisioned != 10737418240 || c.UtilizationPercent != 10 {
		t.Errorf("provisioned = %d, util = %d", c.Provisioned, c.UtilizationPercent)
	}
	if want := time.Unix(1788384958, 0).UTC(); !c.SampledAt.Equal(want) {
		t.Errorf("SampledAt = %s, want %s", c.SampledAt, want)
	}
	if !c.Sampled() {
		t.Error("a sample carrying a date should report Sampled")
	}

	// One query, not six: a caller assembling one sample must not be handed
	// values read at six different instants.
	if len(api.queries) != 1 {
		t.Fatalf("issued %d queries, want 1: %v", len(api.queries), api.queries)
	}
}

// The query has to name every metric and pin the cluster, or it would return
// another cluster's volumes on a multi-cluster Prometheus.
func TestVolumeCapacityQueryNamesEveryMetricAndPinsTheCluster(t *testing.T) {
	api := &stubAPI{}
	if _, err := NewWithAPI(api).VolumeCapacity(context.Background(), testCluster); err != nil {
		t.Fatalf("VolumeCapacity: %v", err)
	}
	q := api.queries[0]
	for _, want := range []string{
		"lvol_size_total", "lvol_size_used", "lvol_size_free",
		"lvol_size_prov", "lvol_size_util", "lvol_date", testCluster,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q does not mention %q", q, want)
		}
	}
}

// The control plane reports date 0 for a volume it has never sampled, and the
// epoch is not a reading. Reporting 1970 as the sample time is how a stale
// mirror looks like a fresh one.
func TestAZeroDateIsNoSampleRatherThanTheEpoch(t *testing.T) {
	api := &stubAPI{vector: model.Vector{
		sample("lvol_size_used", volumeLabels(), 0),
		sample("lvol_date", volumeLabels(), 0),
	}}

	got, err := NewWithAPI(api).VolumeCapacity(context.Background(), testCluster)
	if err != nil {
		t.Fatalf("VolumeCapacity: %v", err)
	}
	c := got[testVolume]
	if !c.SampledAt.IsZero() {
		t.Errorf("SampledAt = %s, want the zero time", c.SampledAt)
	}
	if c.Sampled() {
		t.Error("a volume with no date should not report Sampled")
	}
}

// Devices are keyed by their own label, so a device sample must not be filed
// under a volume's key or dropped for lacking one.
func TestDeviceCapacityIsKeyedByTheDeviceLabel(t *testing.T) {
	labels := map[string]string{
		"cluster": testCluster,
		"device":  testDevice,
		"snode":   "fd687dfd-9b5d-4eca-8cb1-23bcf550ad21",
	}
	api := &stubAPI{vector: model.Vector{
		sample("device_size_used", labels, 888143872),
		sample("device_date", labels, 1788384958),
	}}

	got, err := NewWithAPI(api).DeviceCapacity(context.Background(), testCluster)
	if err != nil {
		t.Fatalf("DeviceCapacity: %v", err)
	}
	if got[testDevice].Used != 888143872 {
		t.Errorf("device used = %d, want 888143872", got[testDevice].Used)
	}
	if !strings.Contains(api.queries[0], "device_size_used") {
		t.Errorf("query %q does not ask for the device metrics", api.queries[0])
	}
}

// A series without the identifying label cannot be attributed to anything, and
// filing it under the empty key would invent an entity.
func TestASeriesWithNoIdentityLabelIsDropped(t *testing.T) {
	api := &stubAPI{vector: model.Vector{
		sample("lvol_size_used", map[string]string{"cluster": testCluster}, 500),
		sample("lvol_size_used", volumeLabels(), 1080033280),
	}}

	got, err := NewWithAPI(api).VolumeCapacity(context.Background(), testCluster)
	if err != nil {
		t.Fatalf("VolumeCapacity: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d volumes, want only the identified one: %v", len(got), got)
	}
	if _, ok := got[""]; ok {
		t.Error("a series with no lvol label was filed under the empty key")
	}
}

// Load is what a volume is doing in both directions at once, so reads and
// writes are summed rather than reported separately.
func TestVolumeIOSumsReadsAndWrites(t *testing.T) {
	api := &stubAPI{vector: model.Vector{
		sample("lvol_read_io_ps", volumeLabels(), 1482),
		sample("lvol_write_io_ps", volumeLabels(), 617),
		sample("lvol_read_bytes_ps", volumeLabels(), 6070272),
		sample("lvol_write_bytes_ps", volumeLabels(), 2529280),
	}}

	got, err := NewWithAPI(api).VolumeIO(context.Background(), testCluster)
	if err != nil {
		t.Fatalf("VolumeIO: %v", err)
	}
	io := got[testVolume]
	if io.IOPS != 2099 {
		t.Errorf("IOPS = %v, want 2099", io.IOPS)
	}
	if io.ThroughputBytesPerSec != 8599552 {
		t.Errorf("throughput = %v, want 8599552", io.ThroughputBytesPerSec)
	}
}

// Latency is keyed by cluster and then node, because one query may span several
// clusters and a node UUID is only unique within one.
func TestClusterLatenciesGroupsByClusterThenNode(t *testing.T) {
	api := &stubAPI{vector: model.Vector{
		sample("simplyblock_node_fio_write_latency_p50_ns",
			map[string]string{"cluster": "c1", "node": "n1"}, 120000.4),
		sample("simplyblock_node_fio_write_latency_p50_ns",
			map[string]string{"cluster": "c1", "node": "n2"}, 240000.6),
		sample("simplyblock_node_fio_write_latency_p50_ns",
			map[string]string{"cluster": "c2", "node": "n3"}, 90000),
	}}

	got, err := NewWithAPI(api).ClusterLatencies(
		context.Background(), []string{"c1", "c2"}, PercentileP50,
	)
	if err != nil {
		t.Fatalf("ClusterLatencies: %v", err)
	}
	if got["c1"]["n1"] != 120000 || got["c1"]["n2"] != 240001 {
		t.Errorf("c1 = %v, want n1 120000 and n2 240001 (rounded)", got["c1"])
	}
	if got["c2"]["n3"] != 90000 {
		t.Errorf("c2 = %v", got["c2"])
	}
	// Several clusters have to be a regular-expression matcher, or only the
	// first would be returned.
	if !strings.Contains(api.queries[0], "c1|c2") {
		t.Errorf("multi-cluster query %q does not match both clusters", api.queries[0])
	}
}

// An unrecognized percentile reads the stable signal. A placement decision
// should not fail because a percentile was misspelled in a CR.
func TestAnUnknownPercentileFallsBackToTheStableSignal(t *testing.T) {
	api := &stubAPI{}
	if _, err := NewWithAPI(api).ClusterLatencies(
		context.Background(), []string{"c1"}, "p42",
	); err != nil {
		t.Fatalf("ClusterLatencies: %v", err)
	}
	if !strings.Contains(api.queries[0], "p50") {
		t.Errorf("query %q did not fall back to p50", api.queries[0])
	}
}

// No clusters is not a query. Asking Prometheus for an empty matcher would
// return every cluster it knows.
func TestNoClustersIssuesNoQuery(t *testing.T) {
	api := &stubAPI{}
	got, err := NewWithAPI(api).ClusterLatencies(context.Background(), nil, PercentileP50)
	if err != nil {
		t.Fatalf("ClusterLatencies: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("returned %v, want empty", got)
	}
	if len(api.queries) != 0 {
		t.Errorf("issued %v, want no query at all", api.queries)
	}
}

// A Prometheus that cannot be reached is a probe that has not reported yet, and
// a caller waits for it rather than failing. The distinction is carried by a
// sentinel so that errors.Is crosses the package boundary.
func TestAnUnreachablePrometheusReportsLatencyNotReady(t *testing.T) {
	api := &stubAPI{err: &promv1.Error{Type: promv1.ErrClient, Msg: "connection refused"}}

	_, err := NewWithAPI(api).ClusterLatencies(
		context.Background(), []string{"c1"}, PercentileP50,
	)
	if !errors.Is(err, ErrLatencyDataNotReady) {
		t.Fatalf("error = %v, want it to wrap ErrLatencyDataNotReady", err)
	}
}

// Any other Prometheus failure is a real error and must not be reported as a
// missing sample, or a broken query would look like a cluster still warming up.
func TestAQueryFailureIsNotReportedAsMissingData(t *testing.T) {
	api := &stubAPI{err: &promv1.Error{Type: promv1.ErrBadData, Msg: "parse error"}}

	_, err := NewWithAPI(api).ClusterLatencies(
		context.Background(), []string{"c1"}, PercentileP50,
	)
	if err == nil {
		t.Fatal("a bad query should fail")
	}
	if errors.Is(err, ErrLatencyDataNotReady) {
		t.Errorf("bad data reported as not-ready: %v", err)
	}
}

// A result that is not a vector means the query was not the one this package
// thinks it issues, which is a bug rather than an empty answer.
func TestANonVectorResultIsAnError(t *testing.T) {
	api := &nonVectorAPI{}
	if _, err := NewWithAPI(api).VolumeCapacity(context.Background(), testCluster); err == nil {
		t.Fatal("a scalar result should be rejected")
	}
}

type nonVectorAPI struct{ promv1.API }

func (nonVectorAPI) Query(
	_ context.Context, _ string, _ time.Time, _ ...promv1.Option,
) (model.Value, promv1.Warnings, error) {
	return &model.Scalar{Value: 1}, nil, nil
}

// A node's capacity is keyed by its own label, and the prefix differs from both
// other kinds: getting either wrong would return an empty map against a cluster
// that has the data.
func TestNodeCapacityIsKeyedByTheSnodeLabel(t *testing.T) {
	const testNode = "fd687dfd-9b5d-4eca-8cb1-23bcf550ad21"
	labels := map[string]string{
		"cluster":  testCluster,
		"snode":    testNode,
		"hostname": "vm02_4420",
	}
	api := &stubAPI{vector: model.Vector{
		sample("snode_size_total", labels, 112303538176),
		sample("snode_size_used", labels, 422576128),
		sample("snode_size_free", labels, 111880962048),
		sample("snode_date", labels, 1788423117),
	}}

	got, err := NewWithAPI(api).NodeCapacity(context.Background(), testCluster)
	if err != nil {
		t.Fatalf("NodeCapacity: %v", err)
	}
	c, ok := got[testNode]
	if !ok {
		t.Fatalf("no sample for the node, got keys %v", got)
	}
	if c.Total != 112303538176 || c.Used != 422576128 || c.Free != 111880962048 {
		t.Errorf("total/used/free = %d/%d/%d", c.Total, c.Used, c.Free)
	}
	if !c.Sampled() {
		t.Error("a node carrying a date should report Sampled")
	}
	if !strings.Contains(api.queries[0], "snode_size_used") {
		t.Errorf("query %q does not ask for the node metrics", api.queries[0])
	}
	// The device and volume prefixes must not leak into a node query, or it
	// would match nothing.
	if strings.Contains(api.queries[0], "device_size_used") ||
		strings.Contains(api.queries[0], "lvol_size_used") {
		t.Errorf("query %q mixes entity kinds", api.queries[0])
	}
}
