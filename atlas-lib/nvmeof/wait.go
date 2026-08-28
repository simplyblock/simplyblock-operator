package nvmeof

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

// errAmbiguousSelector marks a match set that no amount of waiting can resolve:
// several distinct namespaces of one and the same subsystem, i.e., an
// under-specified selector rather than a duplicate the kernel still has to reap.
var errAmbiguousSelector = errors.New("selector matches several namespaces of the same subsystem")

// ConnectDevice attaches t through c and returns the local namespace device
// that came up for it, which is what a CSI NodeStage actually needs: Connect
// alone only guarantees a live controller, not that the block device is already
// visible. nsid picks the namespace on a multi-namespace subsystem, and 0 means
// "the subsystem's only namespace."
//
// It is idempotent to the same degree Connect is: an already-attached target
// short-circuits to the device lookup.
func ConnectDevice(
	ctx context.Context,
	c Connector,
	devs nvme.DeviceResolver,
	t Target,
	nsid nvme.NamespaceID,
) (nvme.Device, error) {
	if err := c.Connect(ctx, t); err != nil {
		return nvme.Device{}, err
	}
	return WaitForDevice(ctx, devs, nvme.DeviceSelector{NQN: t.NQN, NSID: nsid})
}

// ConnectMultipathDevice attaches a volume over all of its fabric paths and
// returns the local namespace device that came up for it, the whole of what a
// CSI NodeStage does, in one call. It is the multipath form of ConnectDevice,
// and the one to reach for by default: a volume published over several paths
// that is attached over one is a volume one node failure away from losing I/O.
//
// The paths are established in the order given, which is the control plane's
// priority order (build targets with Targets to keep it). A path that cannot be
// established does not stop the others, and its reason is in the returned
// PathResult, which is returned even on error so a caller can report or retry
// per path. The error is non-nil only when no path came up at all, or when the
// paths came up but no block device followed.
//
// nsid picks the namespace on a multi-namespace subsystem. Pass
// lvol.Connection.NSID. A nsid of 0 means "the subsystem's only namespace."
func ConnectMultipathDevice(
	ctx context.Context,
	c Connector,
	devs nvme.DeviceResolver,
	targets []Target,
	nsid nvme.NamespaceID,
) (nvme.Device, []PathResult, error) {
	if len(targets) == 0 {
		return nvme.Device{}, nil, fmt.Errorf("connect: no targets")
	}
	results, err := c.ConnectPaths(ctx, targets)
	if err != nil {
		return nvme.Device{}, results, err
	}
	dev, err := WaitForDevice(ctx, devs, nvme.DeviceSelector{NQN: targets[0].NQN, NSID: nsid})
	if err != nil {
		return nvme.Device{}, results, err
	}
	return dev, results, nil
}

// WaitForDevice polls devs until the namespace selected by sel is attached and
// returns it. The block device shows up a moment after the controller goes
// live, so a caller that connected and immediately looked it up would race the
// kernel. The wait ends on ctx (deadline or cancellation), and the first probe
// happens before any waiting, so an already-expired ctx still gets one attempt.
// sel must name at least the NQN, because waiting for "any device" is
// meaningless.
//
// Several devices can match at once, which is why this goes through
// nvme.DeviceResolver.ListWithSelector rather than a By* lookup: those return
// the first match and would hide the case below. Multiple matches are harmless
// when they are the same device seen more than once, but a stale namespace left
// behind by an earlier connect to the same NQN is a *different* device, and
// handing back whichever one came first would be the wrong block device, a
// data-corruption-grade mistake. So a divergent match set counts as "not
// settled yet" and is polled again, since the kernel still has to reap the
// stale subsystem. If it never does, the returned error names the conflicting
// devices. The one divergence waiting cannot fix, distinct namespaces of a
// single subsystem where sel needs an NSID or UUID, fails immediately
// instead.
func WaitForDevice(ctx context.Context, devs nvme.DeviceResolver, sel nvme.DeviceSelector) (nvme.Device, error) {
	if sel.NQN == "" {
		return nvme.Device{}, fmt.Errorf("wait for device %s: selector must set the NQN", sel)
	}
	ticker := time.NewTicker(defaultPoll)
	defer ticker.Stop()

	var unsettled error
	for {
		matched, err := devs.ListWithSelector(ctx, sel)
		if err != nil {
			return nvme.Device{}, fmt.Errorf("wait for device %s: %w", sel, err)
		}

		switch {
		case len(matched) == 1:
			return matched[0], nil
		case len(matched) > 1:
			d, err := soleDevice(reachable(matched))
			if err == nil {
				return d, nil
			}
			if errors.Is(err, errAmbiguousSelector) {
				return nvme.Device{}, fmt.Errorf("wait for device %s: %w", sel, err)
			}
			unsettled = err
		}

		select {
		case <-ctx.Done():
			if unsettled != nil {
				return nvme.Device{}, fmt.Errorf("wait for device %s: %w", sel, errors.Join(ctx.Err(), unsettled))
			}
			return nvme.Device{}, fmt.Errorf("wait for device %s: %w", sel, ctx.Err())
		case <-ticker.C:
		}
	}
}

// reachable narrows a match set to the devices that can actually serve I/O,
// which is what separates a freshly connected subsystem from the stale one the
// kernel has yet to reap: the stale head's paths have gone inaccessible, or its
// controller is no longer live. It only ever narrows: a set with nothing
// reachable is returned untouched, so this decides between candidates and never
// discards the caller's only one.
//
// It cannot resolve every duplicate. A stale head can go on reporting an
// optimized path for a while after a migration, and then both candidates look
// reachable and the caller waits rather than guesses. Reachability says which
// devices are usable, not which one this connect created.
func reachable(matched []nvme.Device) []nvme.Device {
	out := make([]nvme.Device, 0, len(matched))
	for _, d := range matched {
		if d.Accessible() {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return matched
	}
	return out
}

// soleDevice returns the single device that every match refers to, or an error
// describing how they diverge. Errors other than errAmbiguousSelector are
// retryable: an unresolvable device node or a stale duplicate can both clear up
// on the next poll.
func soleDevice(matched []nvme.Device) (nvme.Device, error) {
	ids := make([]string, len(matched))
	for i, d := range matched {
		id, err := deviceIdentity(d)
		if err != nil {
			return nvme.Device{}, err
		}
		ids[i] = id
	}

	diverged, oneSubsystem := false, matched[0].Subsystem.ID != ""
	for i := 1; i < len(matched); i++ {
		if ids[i] != ids[0] {
			diverged = true
		}
		if matched[i].Subsystem.ID != matched[0].Subsystem.ID {
			oneSubsystem = false
		}
	}
	if !diverged {
		return matched[0], nil
	}

	if oneSubsystem {
		return nvme.Device{}, fmt.Errorf("%w %s: %s",
			errAmbiguousSelector, matched[0].Subsystem.ID, describeDevices(matched, ids))
	}
	return nvme.Device{}, fmt.Errorf("%d matches resolve to different devices: %s",
		len(matched), describeDevices(matched, ids))
}

// deviceIdentity returns a value identifying the block device behind a
// namespace: the kernel's major:minor when sysfs reported it, otherwise the
// fully resolved device node, which collapses /dev symlinks (a
// /dev/disk/by-id alias, say) onto their target.
func deviceIdentity(d nvme.Device) (string, error) {
	if d.Namespace.Dev != "" {
		return "dev " + d.Namespace.Dev, nil
	}
	if d.Namespace.DevicePath == "" {
		return "", fmt.Errorf("namespace %q has neither dev nor device path: %w", d.Namespace.Name, errs.ErrNotFound)
	}
	resolved, err := filepath.EvalSymlinks(d.Namespace.DevicePath)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", d.Namespace.DevicePath, err)
	}
	return "path " + resolved, nil
}

func describeDevices(matched []nvme.Device, ids []string) string {
	parts := make([]string, len(matched))
	for i, d := range matched {
		parts[i] = fmt.Sprintf("%s (%s, %s)", d.Namespace.DevicePath, d.Subsystem.ID, ids[i])
	}
	return strings.Join(parts, ", ")
}
