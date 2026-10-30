// Tests for the health monitor, driven pass by pass against a fake cluster, a
// fake Prometheus, and a fake control plane.

package node

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/prometheus"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

const hmCluster = "8ffac363-0c46-4714-a71b-f9c0b58a1269"

// fakeIO answers every cluster with the same readings, or with err.
type fakeIO struct {
	readings map[string]prometheus.DeviceIO
	err      error
}

func (f *fakeIO) DeviceIO(context.Context, string, time.Duration) (map[string]prometheus.DeviceIO, error) {
	return f.readings, f.err
}

// fakeHealthAPI serves health logs by device id and records every report.
type fakeHealthAPI struct {
	logs    map[string]controlplane.DeviceHealth
	readErr error
	reads   []string
	reports map[string][]controlplane.DeviceHealthAssessment
}

func (f *fakeHealthAPI) DeviceHealthInfo(_ context.Context, _, _, deviceID string) (controlplane.DeviceHealth, error) {
	f.reads = append(f.reads, deviceID)
	if f.readErr != nil {
		return controlplane.DeviceHealth{}, f.readErr
	}
	return f.logs[deviceID], nil
}

func (f *fakeHealthAPI) ReportDeviceHealthAssessment(
	_ context.Context, _, _, deviceID string, a controlplane.DeviceHealthAssessment,
) error {
	if f.reports == nil {
		f.reports = map[string][]controlplane.DeviceHealthAssessment{}
	}
	f.reports[deviceID] = append(f.reports[deviceID], a)
	return nil
}

func hmDeviceID(i int) string { return fmt.Sprintf("b222222%d-2222-4222-8222-222222222222", i) }

func hmDevice(i int, phase simplyblockv1alpha2.StorageDevicePhase) *simplyblockv1alpha2.StorageDevice {
	return &simplyblockv1alpha2.StorageDevice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "sb", Name: fmt.Sprintf("node-%d-dev", i),
			Labels: map[string]string{simplyblockv1alpha2.DeviceLabelCluster: "production"},
		},
		Spec: simplyblockv1alpha2.StorageDeviceSpec{NodeRef: fmt.Sprintf("node-%d", i), DeviceID: hmDeviceID(i)},
		Status: simplyblockv1alpha2.StorageDeviceStatus{
			Phase: phase, Role: simplyblockv1alpha2.StorageDeviceRoleStorage,
			ClusterID: hmCluster, NodeID: "a1111111-1111-4111-8111-111111111111",
			Capacity: &simplyblockv1alpha2.DeviceCapacity{TotalBytes: ptr.To(tib)},
		},
	}
}

// reading is one device's I/O at a latency in microseconds, on a 2 GHz clock.
func reading(iops, latencyMicros float64) prometheus.DeviceIO {
	return prometheus.DeviceIO{
		WriteOpsPerSec: iops, WriteLatencyTicksPerSec: iops * latencyMicros * 1e-6 * 2e9, TickRateHz: 2e9,
	}
}

// fleetReadings is four devices at the same load, the first at the latency
// given and the rest at 400 microseconds.
func fleetReadings(firstLatencyMicros float64) map[string]prometheus.DeviceIO {
	out := map[string]prometheus.DeviceIO{}
	for i := 0; i < 4; i++ {
		lat := 400.0
		if i == 0 {
			lat = firstLatencyMicros
		}
		out[hmDeviceID(i)] = reading(300, lat)
	}
	return out
}

type hmHarness struct {
	m     *StorageDeviceHealthMonitor
	io    *fakeIO
	api   *fakeHealthAPI
	rec   *events.FakeRecorder
	c     client.Client
	clock time.Time
}

func newHarness(t *testing.T, devices ...*simplyblockv1alpha2.StorageDevice) *hmHarness {
	t.Helper()
	scheme := testsupport.NewScheme(t, simplyblockv1alpha1.AddToScheme, simplyblockv1alpha2.AddToScheme)
	objs := make([]client.Object, 0, len(devices))
	for _, d := range devices {
		objs = append(objs, d)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha2.StorageDevice{}).WithObjects(objs...).Build()
	h := &hmHarness{
		io:    &fakeIO{readings: fleetReadings(400)},
		api:   &fakeHealthAPI{logs: map[string]controlplane.DeviceHealth{}},
		rec:   events.NewFakeRecorder(256),
		c:     c,
		clock: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
	}
	for i := 0; i < 4; i++ {
		h.api.logs[hmDeviceID(i)] = healthyNVMe()
	}
	h.m = &StorageDeviceHealthMonitor{
		Client: c, Recorder: h.rec, IO: h.io, API: h.api,
		Now: func() time.Time { return h.clock },
	}
	return h
}

func (h *hmHarness) pass(t *testing.T) {
	t.Helper()
	if err := h.m.Pass(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	h.clock = h.clock.Add(time.Minute)
}

func (h *hmHarness) device(t *testing.T, i int) *simplyblockv1alpha2.StorageDevice {
	t.Helper()
	var d simplyblockv1alpha2.StorageDevice
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: "sb", Name: fmt.Sprintf("node-%d-dev", i)}, &d); err != nil {
		t.Fatalf("get device %d: %v", i, err)
	}
	return &d
}

func performanceOf(d *simplyblockv1alpha2.StorageDevice) simplyblockv1alpha2.DevicePerformanceVerdict {
	if d.Status.Health == nil || d.Status.Health.Performance == nil {
		return ""
	}
	return d.Status.Health.Performance.Verdict
}

func fourOnline() []*simplyblockv1alpha2.StorageDevice {
	var out []*simplyblockv1alpha2.StorageDevice
	for i := 0; i < 4; i++ {
		out = append(out, hmDevice(i, simplyblockv1alpha2.StorageDevicePhaseOnline))
	}
	return out
}

func TestASlowDeviceIsPublishedAfterItHasHeldAndRecovers(t *testing.T) {
	h := newHarness(t, fourOnline()...)
	h.pass(t) // healthy: every device Ok at once
	if v := performanceOf(h.device(t, 0)); v != simplyblockv1alpha2.DevicePerformanceOk {
		t.Fatalf("after a healthy pass = %q, want Ok", v)
	}
	drainReasons(h.rec)

	h.io.readings = fleetReadings(40000) // 100 times the fleet
	h.pass(t)
	h.pass(t)
	if v := performanceOf(h.device(t, 0)); v != simplyblockv1alpha2.DevicePerformanceOk {
		t.Fatalf("after two slow passes = %s, want still Ok", v)
	}
	h.pass(t)
	d := h.device(t, 0)
	if v := performanceOf(d); v != simplyblockv1alpha2.DevicePerformanceSlow {
		t.Fatalf("after three slow passes = %s, want Slow", v)
	}
	if p := d.Status.Health.Performance; p.LatencyMicroseconds == nil || *p.LatencyMicroseconds != 40000 ||
		p.FleetLatencyMicroseconds == nil || *p.FleetLatencyMicroseconds != 400 {
		t.Errorf("performance = %+v, want the measured latencies", p)
	}
	if !announced(h.rec, DeviceSlowReason) {
		t.Error("no DeviceSlow event")
	}
	reports := h.api.reports[hmDeviceID(0)]
	if len(reports) == 0 || reports[len(reports)-1].Performance.Verdict != "slow" {
		t.Errorf("reports = %+v, want the last one slow", reports)
	}
	if v := performanceOf(h.device(t, 1)); v != simplyblockv1alpha2.DevicePerformanceOk {
		t.Errorf("a healthy sibling = %s, want Ok", v)
	}

	h.io.readings = fleetReadings(400)
	for i := 0; i < DefaultOkAfter-1; i++ {
		h.pass(t)
	}
	if v := performanceOf(h.device(t, 0)); v != simplyblockv1alpha2.DevicePerformanceSlow {
		t.Fatalf("one good pass short of clearing = %s, want still Slow", v)
	}
	h.pass(t)
	if v := performanceOf(h.device(t, 0)); v != simplyblockv1alpha2.DevicePerformanceOk {
		t.Fatalf("after %d good passes = %s, want Ok", DefaultOkAfter, v)
	}
	if !announced(h.rec, DevicePerformanceRecovered) {
		t.Error("no DevicePerformanceRecovered event")
	}
}

// A Prometheus that cannot be read says nothing about the devices, so the
// verdict they carry stands.
func TestAnUnreadablePrometheusLeavesTheVerdict(t *testing.T) {
	slow := hmDevice(0, simplyblockv1alpha2.StorageDevicePhaseOnline)
	slow.Status.Health = &simplyblockv1alpha2.DeviceHealth{Performance: &simplyblockv1alpha2.DevicePerformance{
		Verdict: simplyblockv1alpha2.DevicePerformanceSlow, AssessedAt: &metav1.Time{Time: time.Date(2026, 10, 28, 11, 59, 0, 0, time.UTC)},
	}}
	devices := append([]*simplyblockv1alpha2.StorageDevice{slow}, fourOnline()[1:]...)
	h := newHarness(t, devices...)
	h.io.err = errors.New("connection refused")

	_ = h.m.Pass(context.Background())
	if v := performanceOf(h.device(t, 0)); v != simplyblockv1alpha2.DevicePerformanceSlow {
		t.Errorf("verdict = %s, want Slow kept", v)
	}
}

func TestTheHealthLogIsReadOnItsScheduleAndJudged(t *testing.T) {
	h := newHarness(t, fourOnline()...)
	failing := healthyNVMe()
	failing.CriticalWarning = ptr.To(0x04)
	h.api.logs[hmDeviceID(2)] = failing

	h.pass(t)
	d := h.device(t, 2)
	if d.Status.Health == nil || d.Status.Health.SMART == nil ||
		d.Status.Health.SMART.Verdict != simplyblockv1alpha2.DeviceSMARTFailing {
		t.Fatalf("smart = %+v, want Failing", d.Status.Health)
	}
	if !announced(h.rec, DeviceSMARTFailingReason) {
		t.Error("no DeviceSMARTFailing event")
	}
	reports := h.api.reports[hmDeviceID(2)]
	if len(reports) == 0 || reports[0].SMART == nil || reports[0].SMART.Verdict != "failing" {
		t.Errorf("reports = %+v, want the SMART verdict reported", reports)
	}

	reads := len(h.api.reads)
	h.pass(t)
	if len(h.api.reads) != reads {
		t.Errorf("a second pass a minute later read %d more logs, want none before the schedule", len(h.api.reads)-reads)
	}
	h.clock = h.clock.Add(DefaultSMARTInterval)
	h.pass(t)
	if len(h.api.reads) != reads+4 {
		t.Errorf("after the interval %d logs were read, want all 4 again", len(h.api.reads)-reads)
	}
}

// A control plane that cannot be reached says nothing about the drive.
// Clearing a Failing verdict for that reason would resolve the alert it backs.
func TestAFailedReadKeepsTheVerdict(t *testing.T) {
	h := newHarness(t, fourOnline()...)
	failing := healthyNVMe()
	failing.CriticalWarning = ptr.To(0x08)
	h.api.logs[hmDeviceID(1)] = failing
	h.pass(t)

	h.api.readErr = errors.New("storage node unreachable")
	h.clock = h.clock.Add(DefaultSMARTInterval)
	_ = h.m.Pass(context.Background())

	d := h.device(t, 1)
	if d.Status.Health.SMART.Verdict != simplyblockv1alpha2.DeviceSMARTFailing {
		t.Errorf("verdict = %s, want Failing kept", d.Status.Health.SMART.Verdict)
	}
	if !announced(h.rec, DeviceHealthCheckFailedReason) {
		t.Error("no DeviceHealthCheckFailed event")
	}
}

func TestAnUnchangedReportIsResentOnlyOnRefresh(t *testing.T) {
	h := newHarness(t, fourOnline()...)
	h.pass(t)
	first := len(h.api.reports[hmDeviceID(0)])
	if first != 1 {
		t.Fatalf("first pass sent %d reports, want 1", first)
	}
	h.pass(t)
	if n := len(h.api.reports[hmDeviceID(0)]); n != first {
		t.Errorf("an unchanged report was resent after a minute (%d)", n)
	}
	h.clock = h.clock.Add(DefaultHealthReportRefresh)
	h.pass(t)
	if n := len(h.api.reports[hmDeviceID(0)]); n != first+1 {
		t.Errorf("after the refresh interval %d reports, want %d", n, first+1)
	}
	last := h.api.reports[hmDeviceID(0)][first]
	if last.Valid != DefaultHealthReportValidity || last.Reporter != healthReporter {
		t.Errorf("report = %+v", last)
	}
}

// A device that is not serving is the control plane's to report: its own
// device_unavailable alert covers it, and one fault must not be two alerts.
func TestADeviceThatIsNotServingIsNotAssessed(t *testing.T) {
	devices := fourOnline()
	devices[3] = hmDevice(3, simplyblockv1alpha2.StorageDevicePhaseFailed)
	h := newHarness(t, devices...)
	h.pass(t)

	if d := h.device(t, 3); d.Status.Health != nil {
		t.Errorf("health = %+v, want nothing for a failed device", d.Status.Health)
	}
	for _, id := range h.api.reads {
		if id == hmDeviceID(3) {
			t.Error("the health log of a failed device was read")
		}
	}
	if _, ok := h.api.reports[hmDeviceID(3)]; ok {
		t.Error("a failed device was reported")
	}
}

// A slow device must not read as fine for the first minutes after the operator
// restarts: the new monitor has no history, and the published verdict is it.
func TestARestartedMonitorKeepsThePublishedVerdict(t *testing.T) {
	slow := hmDevice(0, simplyblockv1alpha2.StorageDevicePhaseOnline)
	slow.Status.Health = &simplyblockv1alpha2.DeviceHealth{Performance: &simplyblockv1alpha2.DevicePerformance{
		Verdict: simplyblockv1alpha2.DevicePerformanceSlow,
	}}
	h := newHarness(t, append([]*simplyblockv1alpha2.StorageDevice{slow}, fourOnline()[1:]...)...)
	h.io.readings = fleetReadings(400)
	h.pass(t)
	if v := performanceOf(h.device(t, 0)); v != simplyblockv1alpha2.DevicePerformanceSlow {
		t.Errorf("verdict after one good pass = %s, want Slow kept", v)
	}
}
