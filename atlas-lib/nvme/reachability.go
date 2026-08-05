package nvme

// Reachability ranks, worst to best. More than one device can answer a single
// lookup — most often a stale subsystem the kernel has not reaped yet sitting
// next to the fresh one for the same NQN, or, with native multipath off, one
// device per path to the same volume — and these order the candidates by how
// much of the I/O path is actually up.
const (
	rankUnreachable = iota // nothing can serve I/O to it right now
	rankLive               // no ANA view, but a controller that owns it is live
	rankAccessible         // an accessible path: I/O allowed, not preferred
	rankOptimized          // a path the target marks optimized
)

// rank scores how serviceable the device is. A namespace with an ANA view is
// judged on its paths alone: if none of them is accessible, no controller
// liveness makes the device usable. Only a namespace without paths — a
// controller's private device under nvme_core.multipath=0, or a head whose legs
// the kernel has yet to publish — falls back to controller state.
func (d Device) rank() int {
	if len(d.Namespace.Paths) > 0 {
		best := rankUnreachable
		for _, p := range d.Namespace.Paths {
			switch {
			case p.ANAState == ANAOptimized:
				return rankOptimized
			case p.ANAState.Accessible():
				best = rankAccessible
			}
		}
		return best
	}
	if d.hasLiveController() {
		return rankLive
	}
	return rankUnreachable
}

// Accessible reports whether I/O can be issued to this device: it has an ANA
// path in an accessible state, or — where the kernel publishes no per-path ANA
// view — a live controller behind it. An attached device can still be
// inaccessible: a stale subsystem awaiting reaping, or a volume whose paths
// have all gone into persistent-loss, is present in sysfs yet unusable.
func (d Device) Accessible() bool {
	return d.rank() > rankUnreachable
}

// hasLiveController reports whether a controller that can serve this namespace
// is live — the owning one when the namespace is a controller's private device,
// any of the subsystem's when it is a multipath head.
func (d Device) hasLiveController() bool {
	for _, c := range d.Subsystem.Controllers {
		if d.Namespace.Controller != "" && c.ID != d.Namespace.Controller {
			continue
		}
		if c.IsLive() {
			return true
		}
	}
	return false
}
