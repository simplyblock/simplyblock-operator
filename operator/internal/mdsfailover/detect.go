// Deciding that the active is lost (design-pnfs-mds-standby.md §7.2).
//
// Every condition has to hold continuously for the detection window, so a
// blip of any one signal restarts it. The detector keeps only the instant the
// conditions began to hold, in memory: a new leader has none, which is the
// window restarting at leadership.

package mdsfailover

import "time"

// Conditions are the facts one pass observed about the active.
type Conditions struct {
	// Planned is a pod with a deletion timestamp.
	Planned bool
	// Rebooted is a recorded boot that ended while its pod's guest is back and
	// reports the standby role, which the guest itself proves.
	Rebooted bool
	// NoSession is no csi-link session for the active.
	NoSession bool
	// Unready is the active's pod or its node not being Ready.
	Unready bool
	// StandbyUp is a standby with a session that reports the standby role.
	StandbyUp bool
}

// Verdict is the detector's answer for one observation.
type Verdict int

const (
	// VerdictHealthy is an active that is not lost.
	VerdictHealthy Verdict = iota
	// VerdictHeld is a loss observed whose window has not elapsed.
	VerdictHeld
	// VerdictLost is a loss to act on.
	VerdictLost
)

// Detector applies the detection window.
type Detector struct {
	window time.Duration
	since  time.Time
}

// NewDetector returns a detector with the given window.
func NewDetector(window time.Duration) *Detector {
	return &Detector{window: window}
}

// Reset forgets the loss observed so far. It is what leadership and a failed
// read do: neither lets the elapsed time count.
func (d *Detector) Reset() { d.since = time.Time{} }

// Judge folds one observation in.
func (d *Detector) Judge(now time.Time, c Conditions) Verdict {
	if c.Planned || c.Rebooted {
		d.Reset()
		return VerdictLost
	}
	if !c.NoSession || !c.Unready || !c.StandbyUp {
		d.Reset()
		return VerdictHealthy
	}
	if d.since.IsZero() {
		d.since = now
	}
	if now.Sub(d.since) > d.window {
		return VerdictLost
	}
	return VerdictHeld
}
