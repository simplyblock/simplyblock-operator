// The node resolving a held volume without the operator: the watchdog that
// fires once a suspend's deadline and grace have passed, and the startup scan
// that finds mappings a previous process suspended and never resumed
// (design-cg-live-subsystem-flip.md §6.5 and §6.6).
//
// A suspended mapping is kernel state. It outlives the process that suspended
// it, and a pod whose volume stays suspended hangs with no error to act on, so
// both paths end with the mapping running: on whichever subsystem holds the
// namespace, or on its current table when none does, where the consumer gets an
// I/O error instead of a hang.

package swaprpc

import (
	"context"
	"fmt"
	"strings"

	"github.com/simplyblock/atlas/nvme"
)

// fire is the watchdog of one volume.
func (s *Swapper) fire(handle string) {
	s.mu.Lock()
	_, held := s.held[handle]
	target := s.targets[handle]
	s.mu.Unlock()
	if !held {
		return
	}
	defer s.release(handle)

	ctx := context.Background()
	v, err := s.load(handle)
	if err != nil || !v.swappable() {
		return
	}
	suspended, err := s.cfg.Mapper.Suspended(ctx, v.mapping)
	if err != nil || !suspended {
		return
	}
	s.resolve(ctx, v, target)
}

// resolve resumes a suspended v on the subsystem holding its namespace,
// preferring target, then the source its record names, then any other.
func (s *Swapper) resolve(ctx context.Context, v volume, target string) {
	outcome := OutcomeNone
	var where string
	found, err := s.cfg.Devices.ListWithSelector(ctx, nvme.DeviceSelector{UUID: v.uuid})
	if err == nil && len(found) > 0 {
		dev, ok := pick(found, target, v.fabric.NQN)
		if ok && s.repoint(ctx, v, dev) == nil {
			where = dev.Subsystem.NQN
			outcome = OutcomeTarget
			if where == v.fabric.NQN {
				outcome = OutcomeSource
			}
		}
	}
	if outcome == OutcomeNone {
		_ = s.cfg.Mapper.Resume(ctx, v.mapping)
	}
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.Watchdog(outcome)
	}
	if s.cfg.Events != nil {
		msg := fmt.Sprintf("Resumed %s on its own after nobody resumed it before its deadline", v.mapping)
		if where != "" {
			msg += " (namespace under " + where + ")"
		} else {
			msg += "; its namespace is under no connected subsystem, so I/O will fail until it is restaged"
		}
		s.cfg.Events.Warning(v.handle, ReasonWatchdogFired, msg)
	}
}

// pick chooses among the devices carrying one namespace: the one under target,
// then the one under source, then any.
func pick(found []nvme.Device, target, source string) (nvme.Device, bool) {
	for _, nqn := range []string{target, source} {
		if nqn == "" {
			continue
		}
		var under []nvme.Device
		for _, d := range found {
			if d.Subsystem.NQN == nqn {
				under = append(under, d)
			}
		}
		if len(under) > 0 {
			return best(under), true
		}
	}
	return best(found), true
}

// Recover resolves every indirection mapping left suspended, as the watchdog
// would. A node plugin calls it once at startup, before it serves anything.
func (s *Swapper) Recover(ctx context.Context) error {
	mappings, err := s.cfg.Mapper.List(ctx, mappingPrefix)
	if err != nil {
		return err
	}
	var suspended []string
	for _, m := range mappings {
		if m.Suspended {
			suspended = append(suspended, m.Name)
		}
	}
	if len(suspended) == 0 {
		return nil
	}
	records, err := s.cfg.Records.List()
	if err != nil {
		return err
	}
	byMapping := map[string]volume{}
	for _, rec := range records {
		v, err := s.load(rec.VolumeHandle)
		if err == nil && v.swappable() {
			byMapping[v.mapping] = v
		}
	}
	for _, name := range suspended {
		v, ok := byMapping[name]
		if !ok {
			v = volume{mapping: name, uuid: strings.TrimPrefix(name, mappingPrefix), fabricI: -1}
		}
		s.resolve(ctx, v, "")
	}
	return nil
}
