// The planned cases of a takeover: a template change rolled one guest at a
// time, a manual switchover, and the switchover a scale-down needs
// (design-pnfs-mds-standby.md §7.4, §4.1).
//
// All three end the same way: the active's pod is deleted, which is the planned
// loss the pass already handles, and the standby to promote is chosen first.

package mdsfailover

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotCandidate is a switchover target that cannot be promoted now.
var ErrNotCandidate = errors.New("the target is not a ready standby")

// ErrNoRemainingStandby is a scale-down that has no standby to move the active
// to, so it must not remove the active's ordinal.
var ErrNoRemainingStandby = errors.New("no ready standby remains on a lower ordinal")

// rollout deletes at most one outdated pod per pass, standbys before the
// active, and only while every other guest is settled.
func (f *Failover) rollout(ctx context.Context, obs Observation, active Guest) error {
	if obs.UpdateRevision == "" || len(obs.Guests) < obs.Replicas {
		return nil
	}
	var standbys []Guest
	for _, g := range obs.Guests {
		if g.Deleting {
			return nil
		}
		if g.Pod != active.Pod {
			standbys = append(standbys, g)
		}
	}
	// A replacement on the new template has to report Standby before the next
	// guest goes, so a rollout never leaves the cluster without one.
	var outdated []Guest
	for _, g := range standbys {
		switch {
		case g.Revision != obs.UpdateRevision:
			outdated = append(outdated, g)
		case !Candidate(g):
			return nil
		}
	}
	if len(outdated) > 0 {
		last := outdated[len(outdated)-1]
		return f.deps.Pods.Delete(ctx, last.Pod)
	}
	if active.Revision != obs.UpdateRevision {
		return f.deps.Pods.Delete(ctx, active.Pod)
	}
	return nil
}

// Switchover deletes the active's pod, which is the planned case, and prefers
// target as the standby to promote. An empty target leaves the choice to the
// pass. A target that is not a candidate is refused.
func (f *Failover) Switchover(ctx context.Context, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	obs, err := f.observe(ctx)
	if err != nil {
		return err
	}
	return f.switchover(ctx, obs, target)
}

func (f *Failover) observe(ctx context.Context) (Observation, error) {
	obs, err := f.deps.Observer.Observe(ctx)
	if err != nil {
		return Observation{}, fmt.Errorf("observing the metadata server: %w", err)
	}
	f.describe(ctx, &obs)
	return obs, nil
}

func (f *Failover) switchover(ctx context.Context, obs Observation, target string) error {
	active, found := obs.guest(obs.Record.Active)
	if obs.Record.Empty() || !found {
		return fmt.Errorf("there is no active to switch over from: %w", ErrNotCandidate)
	}
	if target != "" {
		g, ok := obs.guest(target)
		if !ok || g.Pod == active.Pod || !Candidate(g) {
			return fmt.Errorf("%s: %w", target, ErrNotCandidate)
		}
	}
	f.preferred = target
	if err := f.deps.Pods.Delete(ctx, active.Pod); err != nil {
		f.preferred = ""
		return fmt.Errorf("deleting the active %s: %w", active.Pod, err)
	}
	return nil
}

// EnsureActiveBelow reports whether the active runs on an ordinal below
// replicas, and starts a switchover to a standby that does when it does not.
// The workload's scale-down calls it until it reports true.
func (f *Failover) EnsureActiveBelow(ctx context.Context, replicas int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obs, err := f.observe(ctx)
	if err != nil {
		return false, err
	}
	active, found := obs.guest(obs.Record.Active)
	if obs.Record.Empty() || !found || active.Ordinal < replicas {
		return true, nil
	}
	if active.Deleting {
		return false, nil
	}
	var remaining []Guest
	for _, g := range obs.Guests {
		if g.Ordinal < replicas {
			remaining = append(remaining, g)
		}
	}
	target, ok := ChooseTarget(remaining, active.Zone, "")
	if !ok {
		return false, ErrNoRemainingStandby
	}
	return false, f.switchover(ctx, obs, target.Pod)
}
