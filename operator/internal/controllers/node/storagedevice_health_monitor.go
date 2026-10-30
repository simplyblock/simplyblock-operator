// The device health monitor: every device measured against its fleet on a
// timer, every device's health log read on a slower one, and both verdicts
// published on the device, as gauges, as events, and to the control plane so
// that its alerts endpoint raises them.
//
// It is a Runnable rather than a reconciler for the collector's reason: the
// fleet is the unit of the comparison, so one pass has to see every device of a
// cluster at once, and nothing about a device changing is what should trigger
// it. It writes an object only when a verdict changes or a published reading has
// grown stale, because the readings move continuously and an etcd write per
// sample would wake every watcher of the kind for nothing.

package node

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/prometheus"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The monitor's defaults. Each is overridable on the monitor itself, and the
// operator's flags set them.
const (
	// DefaultDeviceHealthInterval is how often the fleet is assessed.
	DefaultDeviceHealthInterval = time.Minute
	// DefaultDeviceHealthWindow is what each assessment averages over: long
	// enough to hold many of the control plane's fifteen-second samples, short
	// enough that a device going slow shows within minutes.
	DefaultDeviceHealthWindow = 10 * time.Minute
	// DefaultSMARTInterval is how often each device's health log is read. Wear
	// and error counts move over days, so this is hours; a CheckHealth
	// operation reads one now.
	DefaultSMARTInterval = 6 * time.Hour
	// DefaultSMARTRetry is how soon a failed read is tried again.
	DefaultSMARTRetry = 15 * time.Minute
	// DefaultSMARTReadsPerPass bounds the reads one pass issues, so a fleet
	// whose schedule comes due at once (an operator start) is read over a few
	// minutes rather than in one burst against the control plane.
	DefaultSMARTReadsPerPass = 10
	// DefaultSlowAfter and DefaultOkAfter are how many consecutive assessments
	// it takes to turn a device slow and to clear it again.
	DefaultSlowAfter = 3
	DefaultOkAfter   = 5
	// DefaultHealthReportValidity is how long the control plane believes a
	// report. An operator that stops reporting lets its alerts age out after
	// this, rather than leaving them firing forever.
	DefaultHealthReportValidity = 15 * time.Minute
	// DefaultHealthReportRefresh is how often an unchanged report is sent
	// again, comfortably inside its validity.
	DefaultHealthReportRefresh = 5 * time.Minute
	// performanceRefresh is how stale the published measurements may grow
	// before the object is written even though the verdict has not changed.
	performanceRefresh = 15 * time.Minute

	// healthReporter names the operator in the reports it sends.
	healthReporter = "simplyblock-operator"
)

// Event reasons the monitor emits on a device.
const (
	DeviceSlowReason              = "DeviceSlow"
	DevicePerformanceRecovered    = "DevicePerformanceRecovered"
	DeviceSMARTWarningReason      = "DeviceSMARTWarning"
	DeviceSMARTFailingReason      = "DeviceSMARTFailing"
	DeviceSMARTRecovered          = "DeviceSMARTRecovered"
	DeviceHealthCheckFailedReason = "DeviceHealthCheckFailed"
)

// DeviceIOSource supplies the fleet's I/O readings. It is satisfied by
// atlas-lib's prometheus.Provider, and nil disables the performance assessment.
type DeviceIOSource interface {
	DeviceIO(ctx context.Context, clusterUUID string, window time.Duration) (map[string]prometheus.DeviceIO, error)
}

// DeviceHealthAPI is what the monitor and the CheckHealth operation ask of the
// control plane: the health log, and a place to record the verdicts.
type DeviceHealthAPI interface {
	DeviceHealthInfo(ctx context.Context, clusterID, nodeID, deviceID string) (controlplane.DeviceHealth, error)
	ReportDeviceHealthAssessment(ctx context.Context, clusterID, nodeID, deviceID string,
		a controlplane.DeviceHealthAssessment) error
}

// StorageDeviceHealthMonitor assesses every device's performance and health.
type StorageDeviceHealthMonitor struct {
	client.Client
	Recorder events.EventRecorder

	// I/O is where the fleet's readings come from. Nil skips the performance
	// assessment and leaves every published performance verdict as it was.
	IO DeviceIOSource
	// API reads health logs and receives the reports. Nil skips both.
	API DeviceHealthAPI

	Interval          time.Duration
	Window            time.Duration
	SMARTInterval     time.Duration
	SMARTRetry        time.Duration
	SMARTReadsPerPass int
	SlowAfter         int
	OkAfter           int
	ReportValidity    time.Duration
	ReportRefresh     time.Duration
	Fleet             *fleetConfig

	// The clock the monitor reads, which a test replaces.
	Now func() time.Time

	tracker   *verdictTracker
	reported  map[string]sentReport
	smartFail map[string]time.Time
}

// sentReport is what was last sent for a device and when.
type sentReport struct {
	fingerprint string
	at          time.Time
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagedevices,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagedevices/status,verbs=get;update;patch

// NeedLeaderElection implements manager.LeaderElectionRunnable: the verdicts
// are written to objects and reported to the control plane, and two replicas
// doing both would disagree about when a verdict changed.
func (m *StorageDeviceHealthMonitor) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (m *StorageDeviceHealthMonitor) Start(ctx context.Context) error {
	m.defaults()
	log := logf.FromContext(ctx).WithName("storagedevice-health")
	ticker := time.NewTicker(m.Interval)
	defer ticker.Stop()
	for {
		if err := m.Pass(ctx); err != nil {
			log.Error(err, "assessing storage device health")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (m *StorageDeviceHealthMonitor) defaults() {
	if m.Interval == 0 {
		m.Interval = DefaultDeviceHealthInterval
	}
	if m.Window == 0 {
		m.Window = DefaultDeviceHealthWindow
	}
	if m.SMARTInterval == 0 {
		m.SMARTInterval = DefaultSMARTInterval
	}
	if m.SMARTRetry == 0 {
		m.SMARTRetry = DefaultSMARTRetry
	}
	if m.SMARTReadsPerPass == 0 {
		m.SMARTReadsPerPass = DefaultSMARTReadsPerPass
	}
	if m.SlowAfter == 0 {
		m.SlowAfter = DefaultSlowAfter
	}
	if m.OkAfter == 0 {
		m.OkAfter = DefaultOkAfter
	}
	if m.ReportValidity == 0 {
		m.ReportValidity = DefaultHealthReportValidity
	}
	if m.ReportRefresh == 0 {
		m.ReportRefresh = DefaultHealthReportRefresh
	}
	if m.Fleet == nil {
		cfg := defaultFleetConfig()
		m.Fleet = &cfg
	}
	if m.Now == nil {
		m.Now = time.Now
	}
	if m.tracker == nil {
		m.tracker = newVerdictTracker(m.SlowAfter, m.OkAfter)
	}
	if m.reported == nil {
		m.reported = map[string]sentReport{}
	}
	if m.smartFail == nil {
		m.smartFail = map[string]time.Time{}
	}
}

// Pass runs one assessment of every device. It is exported for the test that
// drives the monitor pass by pass.
func (m *StorageDeviceHealthMonitor) Pass(ctx context.Context) error {
	m.defaults()
	var devices simplyblockv1alpha2.StorageDeviceList
	if err := m.List(ctx, &devices); err != nil {
		return fmt.Errorf("list storage devices: %w", err)
	}
	present := make(map[string]bool, len(devices.Items))
	for i := range devices.Items {
		present[deviceKey(&devices.Items[i])] = true
	}
	m.tracker.forget(present)
	for key := range m.reported {
		if !present[key] {
			delete(m.reported, key)
		}
	}

	resetDeviceHealthMetrics()
	var failures []error
	if err := m.assessPerformance(ctx, devices.Items); err != nil {
		failures = append(failures, err)
	}
	if err := m.readDueHealthLogs(ctx, devices.Items); err != nil {
		failures = append(failures, err)
	}
	for i := range devices.Items {
		publishDeviceHealth(&devices.Items[i])
	}
	m.report(ctx, devices.Items)
	return errors.Join(failures...)
}

// deviceKey identifies a device across passes. It is the object's key rather
// than the control-plane id, because a replacement arriving under a new id is a
// new object and must not inherit a verdict.
func deviceKey(device *simplyblockv1alpha2.StorageDevice) string {
	return client.ObjectKeyFromObject(device).String()
}

// assessed reports whether a device is one the fleet comparison and the
// health-log schedule cover: a serving device whose cluster is known. A device
// that is failed, removed, or not observable is the control plane's to report,
// and measuring it would only say that it does not serve.
func assessed(device *simplyblockv1alpha2.StorageDevice) bool {
	if device.Status.ClusterID == "" || device.Status.NodeID == "" {
		return false
	}
	switch device.Status.Phase {
	case simplyblockv1alpha2.StorageDevicePhaseOnline, simplyblockv1alpha2.StorageDevicePhaseDegraded:
		return true
	default:
		return false
	}
}

// assessPerformance compares every serving device against the others of its
// cluster that carry the same role, and publishes the verdicts that changed.
//
// Journal devices are their own fleet. They carry the journal's small
// sequential writes rather than the data stripes, so against the storage
// devices every one of them would look like an outlier.
func (m *StorageDeviceHealthMonitor) assessPerformance(
	ctx context.Context, devices []simplyblockv1alpha2.StorageDevice,
) error {
	if m.IO == nil {
		return nil
	}
	log := logf.FromContext(ctx).WithName("storagedevice-health")

	type fleetKey struct {
		cluster string
		role    simplyblockv1alpha2.StorageDeviceRole
	}
	fleets := map[fleetKey][]*simplyblockv1alpha2.StorageDevice{}
	for i := range devices {
		device := &devices[i]
		if !assessed(device) {
			continue
		}
		key := fleetKey{device.Status.ClusterID, device.Status.Role}
		fleets[key] = append(fleets[key], device)
	}

	readings := map[string]map[string]prometheus.DeviceIO{}
	var failures []error
	for key, fleet := range fleets {
		io, read := readings[key.cluster]
		if !read {
			var err error
			io, err = m.IO.DeviceIO(ctx, key.cluster, m.Window)
			if err != nil {
				// One unreachable Prometheus costs one cluster's assessment, and
				// the verdicts it published stand until the next pass can read.
				log.Error(err, "reading device IO", "clusterID", key.cluster)
				readings[key.cluster] = nil
				continue
			}
			readings[key.cluster] = io
		}
		if io == nil {
			continue
		}

		samples := make([]fleetSample, 0, len(fleet))
		for _, device := range fleet {
			reading, ok := io[device.Spec.DeviceID]
			if !ok {
				continue
			}
			samples = append(samples, fleetSample{
				ID: deviceKey(device), WeightBytes: deviceTotalBytes(device), IO: reading,
			})
		}
		assessments := assessFleet(samples, *m.Fleet)
		for _, device := range fleet {
			a, ok := assessments[deviceKey(device)]
			if !ok {
				a = fleetAssessment{
					Verdict: simplyblockv1alpha2.DevicePerformanceUnknown,
					Reason:  "Prometheus holds no IO readings for the device",
				}
			}
			publishDeviceRatios(device, a)
			if err := m.publishPerformance(ctx, device, a); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

// publishPerformance folds one assessment into the device's verdict and writes
// the device when the verdict changed or its readings have grown stale.
func (m *StorageDeviceHealthMonitor) publishPerformance(
	ctx context.Context, device *simplyblockv1alpha2.StorageDevice, a fleetAssessment,
) error {
	now := m.Now()
	var published simplyblockv1alpha2.DevicePerformanceVerdict
	var current *simplyblockv1alpha2.DevicePerformance
	if device.Status.Health != nil && device.Status.Health.Performance != nil {
		current = device.Status.Health.Performance
		published = current.Verdict
	}
	stable, changed := m.tracker.observe(deviceKey(device), published, a.Verdict)

	stale := current == nil || current.AssessedAt == nil || now.Sub(current.AssessedAt.Time) >= performanceRefresh
	if !changed && !stale && published == stable {
		return nil
	}

	next := &simplyblockv1alpha2.DevicePerformance{Verdict: stable, AssessedAt: &metav1.Time{Time: now}}
	switch {
	case changed || current == nil || current.Since == nil:
		next.Since = &metav1.Time{Time: now}
	default:
		next.Since = current.Since
	}
	// The reason is the assessment's when it agrees with the verdict. While a
	// verdict is held against the readings, the reason that earned it stays, so
	// the object never says "Slow" over a sentence explaining that it is fine.
	if a.Verdict == stable || current == nil {
		next.Reason = a.Reason
	} else {
		next.Reason = current.Reason
	}
	if a.measured {
		if a.LatencySeconds > 0 {
			next.LatencyMicroseconds = ptr.To(int64(math.Round(a.LatencySeconds * 1e6)))
			next.FleetLatencyMicroseconds = ptr.To(int64(math.Round(a.FleetLatencySeconds * 1e6)))
		}
		next.LatencyRatioPercent = ptr.To(clampInt32(a.LatencyRatio))
		next.IOPSRatioPercent = ptr.To(clampInt32(a.IOPSRatio))
	} else if current != nil {
		next.LatencyMicroseconds, next.FleetLatencyMicroseconds = current.LatencyMicroseconds, current.FleetLatencyMicroseconds
		next.LatencyRatioPercent, next.IOPSRatioPercent = current.LatencyRatioPercent, current.IOPSRatioPercent
	}

	if err := m.writeHealth(ctx, device, func(h *simplyblockv1alpha2.DeviceHealth) { h.Performance = next }); err != nil {
		return err
	}
	if changed {
		switch {
		case stable == simplyblockv1alpha2.DevicePerformanceSlow:
			m.Recorder.Eventf(device, nil, corev1.EventTypeWarning, DeviceSlowReason, DeviceSlowReason, "%s", next.Reason)
		case published == simplyblockv1alpha2.DevicePerformanceSlow:
			m.Recorder.Eventf(device, nil, corev1.EventTypeNormal, DevicePerformanceRecovered, DevicePerformanceRecovered,
				"%s", next.Reason)
		}
	}
	return nil
}

// readDueHealthLogs reads the health log of the devices whose schedule has come
// due, oldest first, at most SMARTReadsPerPass of them.
func (m *StorageDeviceHealthMonitor) readDueHealthLogs(
	ctx context.Context, devices []simplyblockv1alpha2.StorageDevice,
) error {
	if m.API == nil {
		return nil
	}
	now := m.Now()
	type due struct {
		device *simplyblockv1alpha2.StorageDevice
		last   time.Time
	}
	var queue []due
	for i := range devices {
		device := &devices[i]
		if !assessed(device) {
			continue
		}
		var last time.Time
		if h := device.Status.Health; h != nil && h.SMART != nil && h.SMART.CheckedAt != nil {
			last = h.SMART.CheckedAt.Time
		}
		interval := m.SMARTInterval
		if failed, ok := m.smartFail[deviceKey(device)]; ok && failed.After(last) {
			last, interval = failed, m.SMARTRetry
		}
		if !last.IsZero() && now.Sub(last) < interval {
			continue
		}
		queue = append(queue, due{device, last})
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i].last.Before(queue[j].last) })
	if len(queue) > m.SMARTReadsPerPass {
		queue = queue[:m.SMARTReadsPerPass]
	}

	var failures []error
	for _, d := range queue {
		if _, err := m.CheckHealth(ctx, d.device); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// CheckHealth reads one device's health log now and publishes it. A read that
// fails leaves the verdict the device already has, because a control plane that
// cannot be reached says nothing about the drive, and clearing a Failing verdict
// for that reason would resolve the alert it backs. It is what the CheckHealth
// operation calls, and the error it returns is the read's.
func (m *StorageDeviceHealthMonitor) CheckHealth(
	ctx context.Context, device *simplyblockv1alpha2.StorageDevice,
) (*simplyblockv1alpha2.DeviceSMART, error) {
	m.defaults()
	if m.API == nil {
		return nil, errors.New("no control plane is configured to read the health log from")
	}
	now := m.Now()
	var previous *simplyblockv1alpha2.DeviceSMART
	if device.Status.Health != nil {
		previous = device.Status.Health.SMART
	}

	h, readErr := m.API.DeviceHealthInfo(ctx, device.Status.ClusterID, device.Status.NodeID, device.Spec.DeviceID)
	var next *simplyblockv1alpha2.DeviceSMART
	switch {
	case readErr == nil:
		delete(m.smartFail, deviceKey(device))
		next = smartStatus(h)
		next.CheckedAt = &metav1.Time{Time: now}
	case previous != nil:
		m.smartFail[deviceKey(device)] = now
		kept := *previous
		kept.Reason = fmt.Sprintf("the last read failed (%v); the verdict is from the read before it", readErr)
		next = &kept
	default:
		m.smartFail[deviceKey(device)] = now
		next = &simplyblockv1alpha2.DeviceSMART{
			Verdict:   simplyblockv1alpha2.DeviceSMARTUnknown,
			Reason:    fmt.Sprintf("the health log could not be read: %v", readErr),
			CheckedAt: &metav1.Time{Time: now},
		}
	}

	if err := m.writeHealth(ctx, device, func(health *simplyblockv1alpha2.DeviceHealth) { health.SMART = next }); err != nil {
		return nil, errors.Join(readErr, err)
	}
	if readErr != nil {
		if !errors.Is(readErr, errs.ErrNotFound) {
			m.Recorder.Eventf(device, nil, corev1.EventTypeWarning, DeviceHealthCheckFailedReason,
				DeviceHealthCheckFailedReason, "reading the health log failed: %v", readErr)
		}
		return next, readErr
	}
	m.announceSMART(device, previous, next)
	return next, nil
}

// announceSMART emits an event when the health log's verdict changed.
func (m *StorageDeviceHealthMonitor) announceSMART(
	device *simplyblockv1alpha2.StorageDevice, previous, next *simplyblockv1alpha2.DeviceSMART,
) {
	var before simplyblockv1alpha2.DeviceSMARTVerdict
	if previous != nil {
		before = previous.Verdict
	}
	if before == next.Verdict {
		return
	}
	switch next.Verdict {
	case simplyblockv1alpha2.DeviceSMARTFailing:
		m.Recorder.Eventf(device, nil, corev1.EventTypeWarning, DeviceSMARTFailingReason, DeviceSMARTFailingReason,
			"%s", next.Reason)
	case simplyblockv1alpha2.DeviceSMARTWarning:
		m.Recorder.Eventf(device, nil, corev1.EventTypeWarning, DeviceSMARTWarningReason, DeviceSMARTWarningReason,
			"%s", next.Reason)
	case simplyblockv1alpha2.DeviceSMARTOk:
		if before == simplyblockv1alpha2.DeviceSMARTFailing || before == simplyblockv1alpha2.DeviceSMARTWarning {
			m.Recorder.Eventf(device, nil, corev1.EventTypeNormal, DeviceSMARTRecovered, DeviceSMARTRecovered,
				"%s", next.Reason)
		}
	}
}

// writeHealth applies change to the device's health block and writes the
// status, re-reading on a conflict. Only the health block is touched: the rest
// of the status is the mirror's, and this write must not move it.
func (m *StorageDeviceHealthMonitor) writeHealth(
	ctx context.Context, device *simplyblockv1alpha2.StorageDevice, change func(*simplyblockv1alpha2.DeviceHealth),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current simplyblockv1alpha2.StorageDevice
		if err := m.Get(ctx, client.ObjectKeyFromObject(device), &current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.Status.Health == nil {
			current.Status.Health = &simplyblockv1alpha2.DeviceHealth{}
		}
		change(current.Status.Health)
		if err := m.Status().Update(ctx, &current); err != nil {
			return err
		}
		*device = current
		return nil
	})
}

// report sends each device's verdicts to the control plane when they changed or
// the last report is due for a refresh.
func (m *StorageDeviceHealthMonitor) report(ctx context.Context, devices []simplyblockv1alpha2.StorageDevice) {
	if m.API == nil {
		return
	}
	log := logf.FromContext(ctx).WithName("storagedevice-health")
	now := m.Now()
	for i := range devices {
		device := &devices[i]
		if !assessed(device) || device.Status.Health == nil {
			continue
		}
		a := assessmentFor(device.Status.Health, now, m.ReportValidity)
		if a.Performance == nil && a.SMART == nil {
			continue
		}
		fp := fingerprint(a)
		key := deviceKey(device)
		if last, ok := m.reported[key]; ok && last.fingerprint == fp && now.Sub(last.at) < m.ReportRefresh {
			continue
		}
		if err := m.API.ReportDeviceHealthAssessment(ctx,
			device.Status.ClusterID, device.Status.NodeID, device.Spec.DeviceID, a); err != nil {
			// A control plane too old to take the report answers 404 for every
			// device; the verdicts are still on the objects and in the gauges,
			// so this is worth a log line rather than a failed pass.
			log.V(1).Info("reporting device health to the control plane", "device", device.Name, "error", err.Error())
			continue
		}
		m.reported[key] = sentReport{fingerprint: fp, at: now}
	}
}

// assessmentFor is the report the device's health block is sent as.
func assessmentFor(h *simplyblockv1alpha2.DeviceHealth, now time.Time, valid time.Duration) controlplane.DeviceHealthAssessment {
	a := controlplane.DeviceHealthAssessment{Reporter: healthReporter, ReportedAt: now, Valid: valid}
	if p := h.Performance; p != nil && p.Verdict != "" {
		perf := &controlplane.DevicePerformanceAssessment{
			Verdict: cpPerformanceVerdict(p.Verdict), Reason: p.Reason,
			LatencyMicros: float64Of(p.LatencyMicroseconds), FleetLatencyMicros: float64Of(p.FleetLatencyMicroseconds),
		}
		if p.Since != nil {
			perf.Since = ptr.To(p.Since.UTC())
		}
		if p.LatencyRatioPercent != nil {
			perf.LatencyRatio = ptr.To(float64(*p.LatencyRatioPercent) / 100)
		}
		if p.IOPSRatioPercent != nil {
			perf.IOPSRatio = ptr.To(float64(*p.IOPSRatioPercent) / 100)
		}
		a.Performance = perf
	}
	if s := h.SMART; s != nil && s.Verdict != "" {
		smart := &controlplane.DeviceSMARTAssessment{Verdict: cpSMARTVerdict(s.Verdict), Reason: s.Reason}
		if s.CheckedAt != nil {
			smart.CheckedAt = ptr.To(s.CheckedAt.UTC())
		}
		a.SMART = smart
	}
	return a
}

// fingerprint is what decides whether a report differs from the last one sent:
// the verdicts and their reasons, not the timestamps a refresh moves.
func fingerprint(a controlplane.DeviceHealthAssessment) string {
	fp := ""
	if p := a.Performance; p != nil {
		fp += p.Verdict + "|" + p.Reason
	}
	fp += "#"
	if s := a.SMART; s != nil {
		fp += s.Verdict + "|" + s.Reason
	}
	return fp
}

// cpPerformanceVerdict and cpSMARTVerdict spell a verdict the way the control
// plane does.
func cpPerformanceVerdict(v simplyblockv1alpha2.DevicePerformanceVerdict) string {
	switch v {
	case simplyblockv1alpha2.DevicePerformanceOk:
		return "ok"
	case simplyblockv1alpha2.DevicePerformanceSlow:
		return "slow"
	default:
		return unknownValue
	}
}

func cpSMARTVerdict(v simplyblockv1alpha2.DeviceSMARTVerdict) string {
	switch v {
	case simplyblockv1alpha2.DeviceSMARTOk:
		return "ok"
	case simplyblockv1alpha2.DeviceSMARTWarning:
		return "warning"
	case simplyblockv1alpha2.DeviceSMARTFailing:
		return "failing"
	case simplyblockv1alpha2.DeviceSMARTUnsupported:
		return "unsupported"
	default:
		return unknownValue
	}
}

func float64Of(v *int64) *float64 {
	if v == nil {
		return nil
	}
	return ptr.To(float64(*v))
}

func clampInt32(v float64) int32 {
	switch {
	case v >= math.MaxInt32:
		return math.MaxInt32
	case v <= 0:
		return 0
	default:
		return int32(math.Round(v))
	}
}
