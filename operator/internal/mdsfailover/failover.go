// The takeover of a storage cluster's metadata server: one pass decides from
// what it observes, and every pass resumes from the active record and the
// exports' status (design-pnfs-mds-standby.md §7).
//
// Nothing about a takeover lives in memory that a restart would lose, apart
// from timing and the detection window. The record names the target before
// anything is asked of it, so a pass after a restart at any step finds the
// takeover where it was and finishes it. Each pass does one bounded piece of
// work and returns, so Reconcile never waits on a guest.

package mdsfailover

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	atlaslvol "github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

// The cadence of a pass. A takeover in progress comes back quickly, and a
// healthy cluster is looked at every few seconds, since a lost link raises no
// event of its own.
const (
	RequeueBusy = time.Second
	RequeueIdle = 5 * time.Second
	// requeuePromoting waits for a guest that is part way through a Promote.
	requeuePromoting = 2 * time.Second
)

// Deps is what a Failover acts through.
type Deps struct {
	Clock    Clock
	Observer Observer
	Store    Store
	Guests   Guests
	Gateway  Gateway
	Exports  Exports
	Pods     Pods
	Events   Events
	Metrics  *Metrics
	Config   Config
	// HostNQN is the host NQN of one ordinal of the StatefulSet (§5.2).
	HostNQN func(statefulSetUID string, ordinal int) (string, error)
}

// takeover is the in-memory part of one takeover: what only timing and the
// events need. Losing it to a restart costs a metric and a cause, never a step.
type takeover struct {
	cause      Cause
	epoch      uint64
	detectedAt time.Time
	recordedAt time.Time
	// serveAt is when the endpoint was first pointed at the target.
	serveAt       time.Time
	fenced        bool
	failedCounted bool
}

type backoff struct {
	epoch    uint64
	failures int
	until    time.Time
}

// Failover decides and drives the takeovers of one StatefulSet.
type Failover struct {
	// mu serializes passes with the switchovers requested from outside.
	mu        sync.Mutex
	deps      Deps
	detector  *Detector
	pending   *takeover
	backoff   backoff
	preferred string
}

// New returns a Failover.
func New(d Deps) *Failover {
	return &Failover{deps: d, detector: NewDetector(d.Config.window())}
}

func (f *Failover) now() time.Time { return f.deps.Clock.Now() }

// LeadershipAcquired restarts the detection window: a new leader never counts
// time it did not observe.
func (f *Failover) LeadershipAcquired() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detector.Reset()
}

// Pass observes the cluster and does the next piece of the takeover, if one is
// due. An error means the pass learned nothing or could not act, and is worth
// the caller's backoff.
func (f *Failover) Pass(ctx context.Context) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obs, err := f.deps.Observer.Observe(ctx)
	if err != nil {
		// A pass that could not read counts as no observation at all, and the
		// time it took does not count toward a window either.
		f.detector.Reset()
		return Result{}, fmt.Errorf("observing the metadata server: %w", err)
	}
	f.describe(ctx, &obs)
	f.publish(obs)
	if err := f.demote(ctx, obs); err != nil {
		return Result{}, err
	}
	if obs.Record.Empty() {
		return f.first(ctx, obs)
	}
	return f.maintain(ctx, obs)
}

// describe asks every guest with a session for its role. A guest that does not
// answer keeps no role, and is neither a candidate nor active.
func (f *Failover) describe(ctx context.Context, obs *Observation) {
	for i := range obs.Guests {
		g := &obs.Guests[i]
		if !g.HasSession {
			continue
		}
		d, err := f.deps.Guests.Describe(ctx, g.Pod)
		if err != nil {
			continue
		}
		g.Role, g.Epoch = d.Role, d.Epoch
	}
}

func (f *Failover) publish(obs Observation) {
	if f.deps.Metrics == nil {
		return
	}
	roles := make(map[string]nfsexportrpc.Role, len(obs.Guests))
	for _, g := range obs.Guests {
		roles[g.Pod] = g.Role
	}
	f.deps.Metrics.SetRoles(obs.Cluster, roles)
	f.deps.Metrics.SetEpoch(obs.Cluster, obs.Record.Epoch)
}

// demote deletes the pod of a guest that reports the active role under an
// instance the record does not name: a partition that healed, or a paused node
// that resumed (§7.5). The guest is fenced already, so this ends its stale
// mounts and its running nfsd.
func (f *Failover) demote(ctx context.Context, obs Observation) error {
	if obs.Record.Empty() {
		return nil
	}
	for _, g := range obs.Guests {
		if g.Role != nfsexportrpc.RoleActive || g.Deleting || g.Instance == obs.Record.Instance {
			continue
		}
		if err := f.deps.Pods.Delete(ctx, g.Pod); err != nil {
			return fmt.Errorf("demoting %s: %w", g.Pod, err)
		}
		f.deps.Events.Driver(corev1.EventTypeNormal, "MDSDemoted", fmt.Sprintf(
			"%s reported the active role under an earlier boot or epoch than the record's %s at epoch %d, "+
				"so its pod was deleted and restarts as a standby", g.Pod, obs.Record.Active, obs.Record.Epoch))
	}
	return nil
}

// first promotes the first guest of a storage cluster, once an export needs
// one and the state volume exists.
func (f *Failover) first(ctx context.Context, obs Observation) (Result, error) {
	exports, err := f.deps.Exports.List(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("listing exports: %w", err)
	}
	if len(exports) == 0 || obs.StateVolume == "" {
		return Result{RequeueAfter: RequeueIdle}, nil
	}
	return f.choose(ctx, obs, Guest{}, CauseFirst)
}

// maintain judges the recorded active and either starts a takeover, holds, or
// drives the recorded one forward.
func (f *Failover) maintain(ctx context.Context, obs Observation) (Result, error) {
	active, found := obs.guest(obs.Record.Active)
	cond := f.conditions(obs, active, found)
	detectedAt := f.detector.since
	switch f.detector.Judge(f.now(), cond) {
	case VerdictHeld:
		f.deps.Events.Driver(corev1.EventTypeNormal, "MDSFailoverHeld", fmt.Sprintf(
			"the active %s looks lost, and the takeover waits out the %s detection window",
			obs.Record.Active, f.deps.Config.window()))
		return Result{RequeueAfter: RequeueBusy}, nil
	case VerdictLost:
		cause := CauseLost
		if cond.Planned {
			cause = CauseDeleted
		}
		if detectedAt.IsZero() || cond.Planned || cond.Rebooted {
			detectedAt = f.now()
		}
		f.noteDetected(detectedAt)
		return f.choose(ctx, obs, active, cause)
	}
	if cond.NoSession && cond.Unready {
		// A loss with no standby to take over.
		f.deps.Events.Driver(corev1.EventTypeWarning, "MDSStandbyMissing", fmt.Sprintf(
			"the active %s looks lost and no standby with a session reports the standby role", obs.Record.Active))
		return Result{RequeueAfter: RequeueIdle}, nil
	}
	return f.drive(ctx, obs, active, found)
}

// noteDetected remembers when the loss was first observed, for the Detect
// phase, until the record is written.
func (f *Failover) noteDetected(at time.Time) {
	if f.pending == nil {
		f.pending = &takeover{detectedAt: at}
	}
}

// conditions reads the loss conditions of §7.2 off the observation.
func (f *Failover) conditions(obs Observation, active Guest, found bool) Conditions {
	rec := obs.Record
	c := Conditions{
		Planned:   found && active.Deleting,
		NoSession: !found || !active.HasSession,
		Unready:   !found || !active.Ready || !active.NodeReady,
	}
	c.Rebooted = found && !active.Deleting && active.Instance != rec.Instance &&
		active.HasSession && active.Role == nfsexportrpc.RoleStandby
	for _, g := range obs.Guests {
		if g.Pod != rec.Active && g.HasSession && g.Role == nfsexportrpc.RoleStandby {
			c.StandbyUp = true
		}
	}
	return c
}

// choose picks the target of a takeover and records it.
func (f *Failover) choose(ctx context.Context, obs Observation, lost Guest, cause Cause) (Result, error) {
	target, ok := ChooseTarget(obs.Guests, lost.Zone, f.preferred)
	if !ok {
		f.deps.Events.Driver(corev1.EventTypeWarning, "MDSStandbyMissing",
			"no standby is running and ready, so the takeover waits for the first guest that reports the standby role")
		return Result{RequeueAfter: RequeueIdle}, nil
	}
	rec := Record{Active: target.Pod, Instance: target.Instance, Epoch: obs.Record.Epoch + 1}
	return f.record(ctx, obs, rec, target, cause)
}

// record is Step 2: the conditioned write. A conflict ends the pass, and the
// next one reads whoever won.
func (f *Failover) record(ctx context.Context, obs Observation, rec Record, target Guest, cause Cause) (Result, error) {
	if _, err := nfsexport.MDSReservationKey(obs.StatefulSetUID, rec.Epoch); err != nil {
		return Result{}, fmt.Errorf("epoch %d of %s: %w", rec.Epoch, obs.StatefulSet, err)
	}
	version, err := f.deps.Store.Write(ctx, obs.Version, rec)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			f.deps.Events.Driver(corev1.EventTypeNormal, "MDSPromotionConflict", fmt.Sprintf(
				"another writer moved the active record before the promotion of %s at epoch %d, so this pass did not promote",
				rec.Active, rec.Epoch))
			if f.deps.Metrics != nil {
				f.deps.Metrics.Failover(obs.Cluster, cause, ResultConflict)
			}
			return Result{RequeueAfter: RequeueBusy}, nil
		}
		return Result{}, err
	}
	now := f.now()
	t := &takeover{cause: cause, epoch: rec.Epoch, detectedAt: now, recordedAt: now}
	if f.pending != nil && !f.pending.detectedAt.IsZero() {
		t.detectedAt = f.pending.detectedAt
	}
	f.pending = t
	if f.deps.Metrics != nil {
		f.deps.Metrics.Phase(obs.Cluster, PhaseDetect, now.Sub(t.detectedAt))
	}
	f.preferred = ""
	obs.Record, obs.Version = rec, version
	return f.promote(ctx, obs, target)
}

// drive moves a recorded takeover along, or keeps a finished one in order.
func (f *Failover) drive(ctx context.Context, obs Observation, active Guest, found bool) (Result, error) {
	rec := obs.Record
	switch {
	case !found || !active.HasSession || active.Instance != rec.Instance:
		return Result{RequeueAfter: RequeueIdle}, nil
	case active.Role == nfsexportrpc.RolePromoting:
		return Result{RequeueAfter: requeuePromoting}, nil
	case active.Role == nfsexportrpc.RoleActive && active.Epoch == rec.Epoch:
		return f.serve(ctx, obs, active)
	case active.Role == nfsexportrpc.RoleStandby || active.Role == nfsexportrpc.RoleActive:
		return f.promote(ctx, obs, active)
	}
	return Result{RequeueAfter: RequeueIdle}, nil
}

// promote is Steps 3 and 4: withdraw, then ask the target to fence and serve.
func (f *Failover) promote(ctx context.Context, obs Observation, target Guest) (Result, error) {
	rec := obs.Record
	now := f.now()
	if f.backoff.epoch == rec.Epoch && now.Before(f.backoff.until) {
		return Result{RequeueAfter: f.backoff.until.Sub(now)}, nil
	}
	req, err := f.request(ctx, obs, target)
	if err != nil {
		return Result{}, err
	}
	if err := f.deps.Gateway.Withdraw(ctx, target.Pod); err != nil {
		return Result{}, fmt.Errorf("withdrawing the metadata server's address: %w", err)
	}
	f.ensurePending(rec.Epoch)

	start := f.now()
	res, err := f.deps.Guests.Promote(ctx, target.Pod, req)
	if err != nil {
		return Result{}, fmt.Errorf("promoting %s at epoch %d: %w", target.Pod, rec.Epoch, err)
	}
	switch res.Outcome {
	case nfsexportrpc.PromoteActive:
		return f.promoted(ctx, obs, target, req, f.now().Sub(start))
	case nfsexportrpc.PromoteCollision:
		return f.collided(ctx, obs, target)
	case nfsexportrpc.PromoteStaleEpoch:
		return Result{RequeueAfter: RequeueBusy}, nil
	case nfsexportrpc.PromoteSuperseded:
		f.fenceFailed(obs, target, res, "the record names a newer active")
		return Result{RequeueAfter: RequeueBusy}, nil
	default:
		f.fenceFailed(obs, target, res, "")
		return f.backoffRetry(rec.Epoch), nil
	}
}

// request builds the Promote of the record's epoch.
func (f *Failover) request(ctx context.Context, obs Observation, target Guest) (nfsexportrpc.PromoteRequest, error) {
	rec := obs.Record
	key, err := nfsexport.MDSReservationKey(obs.StatefulSetUID, rec.Epoch)
	if err != nil {
		return nfsexportrpc.PromoteRequest{}, err
	}
	handle, ok := atlaslvol.ParseHandle(atlaslvol.VolumeHandle(obs.StateVolume))
	if !ok {
		return nfsexportrpc.PromoteRequest{}, fmt.Errorf("state volume %q is not a volume handle", obs.StateVolume)
	}
	nqn, err := f.deps.HostNQN(obs.StatefulSetUID, target.Ordinal)
	if err != nil {
		return nfsexportrpc.PromoteRequest{}, err
	}
	specs, err := f.deps.Exports.Specs(ctx, nqn)
	if err != nil {
		return nfsexportrpc.PromoteRequest{}, fmt.Errorf("building the exports' specs: %w", err)
	}
	return nfsexportrpc.PromoteRequest{
		Epoch: rec.Epoch, Key: key, HostNQN: nqn, StatefulSetUID: obs.StatefulSetUID,
		StateVolume: nfsexportrpc.VolumeSpec{
			VolumeUUID: handle.VolumeID, ClusterID: handle.ClusterID, PoolID: handle.PoolRef,
		},
		Exports: specs,
	}, nil
}

// ensurePending makes sure a takeover of the epoch is being tracked, as after a
// restart, when only the record knows about it.
func (f *Failover) ensurePending(epoch uint64) {
	if f.pending != nil && f.pending.epoch == epoch {
		return
	}
	now := f.now()
	f.pending = &takeover{cause: CauseLost, epoch: epoch, detectedAt: now, recordedAt: now}
}

func (f *Failover) promoted(
	_ context.Context, obs Observation, target Guest, req nfsexportrpc.PromoteRequest, took time.Duration,
) (Result, error) {
	f.backoff = backoff{}
	if t := f.pending; t != nil && !t.fenced {
		t.fenced = true
		if f.deps.Metrics != nil {
			f.deps.Metrics.Phase(obs.Cluster, PhaseFence, took)
		}
	}
	f.deps.Events.Driver(corev1.EventTypeNormal, "MDSFenced", fmt.Sprintf(
		"%d devices (the state volume and %d exports) are fenced under the key of epoch %d on %s",
		1+len(req.Exports), len(req.Exports), obs.Record.Epoch, target.Pod))
	target.Role, target.Epoch = nfsexportrpc.RoleActive, obs.Record.Epoch
	return f.serve(context.Background(), obs, target)
}

// collided raises the epoch through the record and leaves the retry to the next
// pass.
func (f *Failover) collided(ctx context.Context, obs Observation, target Guest) (Result, error) {
	f.deps.Events.Driver(corev1.EventTypeWarning, "MDSFenceFailed", fmt.Sprintf(
		"another host is registered under the key of epoch %d, so the epoch is raised and %s is promoted again",
		obs.Record.Epoch, target.Pod))
	rec := obs.Record
	rec.Epoch++
	if _, err := nfsexport.MDSReservationKey(obs.StatefulSetUID, rec.Epoch); err != nil {
		return Result{}, err
	}
	if _, err := f.deps.Store.Write(ctx, obs.Version, rec); err != nil {
		if errors.Is(err, ErrConflict) {
			return Result{RequeueAfter: RequeueBusy}, nil
		}
		return Result{}, err
	}
	if f.pending != nil {
		f.pending.epoch = rec.Epoch
	}
	return Result{RequeueAfter: RequeueBusy}, nil
}

func (f *Failover) fenceFailed(obs Observation, target Guest, res nfsexportrpc.PromoteResult, why string) {
	msg := fmt.Sprintf("the promotion of %s at epoch %d mounted nothing: %s failed on %s",
		target.Pod, obs.Record.Epoch, orUnknown(res.Step), orUnknown(res.Device))
	if why != "" {
		msg = fmt.Sprintf("the promotion of %s at epoch %d was refused: %s", target.Pod, obs.Record.Epoch, why)
	}
	if res.Message != "" {
		msg += " (" + res.Message + ")"
	}
	f.deps.Events.Driver(corev1.EventTypeWarning, "MDSFenceFailed", msg)
	f.deps.Events.Exports(corev1.EventTypeWarning, "MDSFenceFailed", msg)
	if t := f.pending; t != nil && !t.failedCounted && f.deps.Metrics != nil {
		t.failedCounted = true
		f.deps.Metrics.Failover(obs.Cluster, t.cause, ResultFenceFailed)
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "an unnamed step or device"
	}
	return s
}

// backoffRetry spaces the retries of a failed promotion at one epoch.
func (f *Failover) backoffRetry(epoch uint64) Result {
	first, limit := f.deps.Config.fenceBackoff()
	if f.backoff.epoch != epoch {
		f.backoff = backoff{epoch: epoch}
	}
	delay := first << f.backoff.failures
	if delay <= 0 || delay > limit {
		delay = limit
	}
	f.backoff.failures++
	f.backoff.until = f.now().Add(delay)
	return Result{RequeueAfter: delay}
}

// serve is Step 5: the endpoint at the target, then the exports handed to it,
// a batch at a time.
func (f *Failover) serve(ctx context.Context, obs Observation, active Guest) (Result, error) {
	if active.PodIP == "" {
		return Result{RequeueAfter: RequeueBusy}, nil
	}
	f.ensurePending(obs.Record.Epoch)
	started := f.now()
	if err := f.deps.Gateway.Serve(ctx, active.Pod, active.PodIP); err != nil {
		return Result{}, fmt.Errorf("pointing the metadata server's address at %s: %w", active.Pod, err)
	}
	if t := f.pending; t != nil && t.serveAt.IsZero() {
		t.serveAt = started
		if f.deps.Metrics != nil {
			f.deps.Metrics.Phase(obs.Cluster, PhaseServe, f.now().Sub(started))
		}
	}

	exports, err := f.deps.Exports.List(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("listing exports: %w", err)
	}
	var inflight int
	var todo []ExportState
	for _, e := range exports {
		switch {
		case e.Pod == active.Pod && e.AssembledBy != active.Instance:
			inflight++
		case e.Pod != "" && e.Pod != active.Pod:
			todo = append(todo, e)
		}
	}
	slices.SortFunc(todo, func(a, b ExportState) int {
		return compareKeys(a, b)
	})
	room := max(f.deps.Config.parallelism()-inflight, 0)
	for _, e := range todo[:min(room, len(todo))] {
		if err := f.deps.Exports.Rebind(ctx, e.Key, active.Pod); err != nil {
			return Result{}, fmt.Errorf("handing export %s to %s: %w", e.Key, active.Pod, err)
		}
	}
	if len(todo) > 0 || inflight > 0 {
		return Result{RequeueAfter: RequeueBusy}, nil
	}
	f.finish(obs, active)
	return f.steady(ctx, obs, active)
}

func compareKeys(a, b ExportState) int {
	if c := cmpString(a.Key.Namespace, b.Key.Namespace); c != 0 {
		return c
	}
	return cmpString(a.Key.Name, b.Key.Name)
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// finish ends the takeover being tracked, once the target serves every export.
func (f *Failover) finish(obs Observation, active Guest) {
	t := f.pending
	if t == nil || t.epoch != obs.Record.Epoch {
		return
	}
	f.pending = nil
	if t.serveAt.IsZero() {
		return
	}
	took := f.now().Sub(t.recordedAt)
	if f.deps.Metrics != nil {
		f.deps.Metrics.Phase(obs.Cluster, PhaseReassemble, f.now().Sub(t.serveAt))
		f.deps.Metrics.Failover(obs.Cluster, t.cause, ResultSucceeded)
	}
	kind := corev1.EventTypeNormal
	if t.cause == CauseLost {
		kind = corev1.EventTypeWarning
	}
	msg := fmt.Sprintf("%s is the active at epoch %d, cause %s, after %s",
		active.Pod, obs.Record.Epoch, t.cause, took.Round(time.Millisecond))
	f.deps.Events.Driver(kind, "MDSPromoted", msg)
	f.deps.Events.Exports(kind, "MDSPromoted", msg)
}

// steady is what a settled cluster owes: a report when no standby runs, and
// the rollout of a changed template.
func (f *Failover) steady(ctx context.Context, obs Observation, active Guest) (Result, error) {
	if obs.Replicas > 1 && !slices.ContainsFunc(obs.Guests, func(g Guest) bool {
		return g.Pod != active.Pod && g.HasSession && g.Role == nfsexportrpc.RoleStandby
	}) {
		f.deps.Events.Driver(corev1.EventTypeWarning, "MDSStandbyMissing",
			"no standby is running and ready, so the exports are unserved after a loss of the active")
	}
	if err := f.rollout(ctx, obs, active); err != nil {
		return Result{}, err
	}
	return Result{RequeueAfter: RequeueIdle, Settled: true}, nil
}
