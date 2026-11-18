// Priming a pNFS client's device lookup, so pods get the direct path at all.
//
// A client resolves the device a SCSI layout names by opening
// /dev/disk/by-id/nvme-eui.<nguid> in the mount namespace of whichever task
// asked for the layout. A pod's /dev is the minimal one kubelet builds, with no
// disk/ in it, so a lookup the application triggers fails, and the failure
// sticks: the device is marked unavailable for two minutes and every byte goes
// through the metadata server. This container has the host's /dev. A probe
// written here takes the first layout, the device it resolves is cached for
// the whole client, and every pod's later layout reuses it.
//
// The cache does not survive a server restart: the client drops its devices
// when the server's state is gone. So the probe runs twice over a mount's life,
// at stage and again after every reconnect to the server, which is how a
// restart shows on the client. The metadata server's nfsd holds the client's
// other layouts until the probe has taken one (nfsexport.LayoutProbeName), so
// the probe is first again rather than racing the application.

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/klog"

	"github.com/simplyblock/atlas/nfsclient"
	export "github.com/simplyblock/atlas/nfsexport"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// scsiLayout is the layout type mountstats reports for a pNFS SCSI mount.
const scsiLayout = "LAYOUT_SCSI"

// primeInterval is how often the mounts are checked for a reconnect. The
// server holds a reconnected client's layouts for the length of its grace
// period and a while after, so seconds are enough to be first.
const primeInterval = 2 * time.Second

// mountWrites is the NFS WRITE count of the mount at path, and false when the
// client reports no NFS mount there. A variable so the tests can set it.
var mountWrites = func(path string) (int64, bool) {
	mounts, err := nfsclient.ReadMountstats()
	if err != nil {
		return 0, false
	}
	for _, m := range mounts {
		if m.MountPoint == path {
			return m.Ops["WRITE"], true
		}
	}
	return 0, false
}

// openProbe opens the probe file. A variable so the tests can see how.
var openProbe = os.OpenFile

// probeLocks runs one probe per staging mount at a time. Stage, expand, and
// the reconnect loop all probe, and two probes of one mount share its probe
// file: one would hold the inode while the server waited for the other's
// layout to come back.
var probeLocks = csicommon.NewVolumeLocks()

// primeLayout triggers a LAYOUTGET from this process rather than a pod. See the
// file comment.
func primeLayout(ctx context.Context, stagingPath string) error {
	// Checked before rather than during: the syscalls below are not
	// cancellable, and a stage that gave up should not add I/O.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("pnfs: not taking the layout for %s: %w", stagingPath, err)
	}

	unlock := probeLocks.Lock(stagingPath)
	defer unlock()
	// Looked at again: waiting for another probe can outlast the stage.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("pnfs: not taking the layout for %s: %w", stagingPath, err)
	}

	writesBefore, counted := mountWrites(stagingPath)

	probe := filepath.Join(stagingPath, export.LayoutProbeName)
	// Never O_TRUNC. A probe another node left behind is overwritten in place,
	// because a size change makes the server recall every layout on the file,
	// this client's own included, and answer NFS4ERR_DELAY until they are back.
	f, err := openProbe(probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("pnfs: opening the layout probe at %s: %w", probe, err)
	}
	// Removed whatever happens: this is the user's filesystem.
	defer func() {
		_ = f.Close()
		_ = os.Remove(probe)
	}()

	// One block, enough to ask for a read-write layout.
	if _, err := f.Write(make([]byte, 4096)); err != nil {
		return fmt.Errorf("pnfs: writing the layout probe: %w", err)
	}
	// Synced: the layout is taken on write-back, not on entering the page
	// cache, and the ordering against pod I/O depends on it.
	if err := f.Sync(); err != nil {
		return fmt.Errorf("pnfs: syncing the layout probe: %w", err)
	}
	// A write that reached the server as an NFS WRITE went through the metadata
	// server: no layout was taken, and the device was not looked up. The server
	// refuses layouts during its grace period, and the write still succeeds, so
	// success alone says nothing. The mount's pods share the count, so a pod's
	// own write through the server fails a probe too, and the next pass probes
	// again.
	if counted {
		if writesAfter, ok := mountWrites(stagingPath); ok && writesAfter > writesBefore {
			return fmt.Errorf("pnfs: the probe of %s went through the metadata server, so no layout "+
				"was taken; the server may still be in its grace period", stagingPath)
		}
	}
	return nil
}

// layoutPrimer decides which staging mounts need a probe. It holds no
// goroutines, so the decision is testable without a mount.
type layoutPrimer struct {
	// stagingMarker is the path segment every staging mount of this driver
	// carries, whatever kubelet's root directory is.
	stagingMarker string

	mu sync.Mutex
	// connects is each primed mount's transport connect count when its
	// probe was started.
	connects map[string]uint64
	running  map[string]bool
}

func newLayoutPrimer(driverName string) *layoutPrimer {
	return &layoutPrimer{
		stagingMarker: "/plugins/kubernetes.io/csi/" + driverName + "/",
		connects:      map[string]uint64{},
		running:       map[string]bool{},
	}
}

// due returns the staging mounts to probe now and marks them running: one this
// process has not seen, and one whose client has reconnected since its last
// probe. A pod's bind of a staging mount is skipped, since it shares the
// staging mount's client, and so is any mount without a SCSI layout.
func (p *layoutPrimer) due(mounts []nfsclient.Mount) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var out []string
	present := map[string]bool{}
	for _, m := range mounts {
		if !p.isStagingMount(m) {
			continue
		}
		present[m.MountPoint] = true
		if p.running[m.MountPoint] {
			continue
		}
		if last, seen := p.connects[m.MountPoint]; seen && last == m.Connects {
			continue
		}
		p.connects[m.MountPoint] = m.Connects
		p.running[m.MountPoint] = true
		out = append(out, m.MountPoint)
	}
	// An unstaged mount is forgotten, so a later stage on the same path is
	// first seen again.
	for path := range p.connects {
		if !present[path] && !p.running[path] {
			delete(p.connects, path)
		}
	}
	return out
}

// done records a probe's end. A failed one is forgotten, so the next pass
// probes the mount again.
func (p *layoutPrimer) done(path string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.running, path)
	if err != nil {
		delete(p.connects, path)
	}
}

func (p *layoutPrimer) isStagingMount(m nfsclient.Mount) bool {
	return m.UsesLayout(scsiLayout) &&
		strings.Contains(m.MountPoint, p.stagingMarker) &&
		strings.Contains(m.MountPoint, "/globalmount")
}

// deviceAlias refreshes the by-id alias the kernel resolves a pNFS block
// layout through. A variable, not a direct call to ensureDeviceAlias, so
// tests can observe a refresh without writing under /dev/disk/by-id.
var deviceAlias = ensureDeviceAlias

// reconcileDeviceAliases re-derives every live pNFS staging mount's current
// (nguid, devicePath) from sysfs and refreshes its alias, every tick,
// regardless of whether this mount's own transport reconnected.
//
// ensureDeviceAlias otherwise only ran once, at NodeStageVolume. A device
// node can change under an already-staged volume without this process
// running any code synchronously with it -- the background NVMe-oF path
// repair in internal/reconnect, or healStack repairing an unhealthy stack on
// a later publish, neither of which touches the alias -- because device
// nodes are assigned in attach order and a reconnect elsewhere on the node
// can hand a freed one to a different namespace entirely. Only re-checking
// ground truth on a schedule catches that (issue 10). See
// docs/device-event-unification-idea.md for the event-driven alternative
// this stands in for, deferred for now because the complexity is not
// justified by the gain: the observed failure was ten days stale, not a
// sub-poll-interval race.
//
// The volume ID each mount resolves against is read off its own mount
// point, this driver's own staging-path convention
// (getStagingTargetPath: stagingParentPath + "/" + volumeID) -- a pNFS stage
// never stashes one, so there is nothing else to read it back from.
func reconcileDeviceAliases(ctx context.Context, primer *layoutPrimer, mounts []nfsclient.Mount) {
	for _, m := range mounts {
		if !primer.isStagingMount(m) {
			continue
		}
		volumeID := filepath.Base(m.MountPoint)
		nguid, devicePath, err := namespaceIdentity(ctx, volumeID)
		if err != nil {
			// No device currently backs this volume: total path loss, or
			// caught between reconnects. Already reported elsewhere
			// (internal/initiator's presence tracking); nothing new to alias.
			klog.V(2).Infof("pnfs: no current device for %s, alias left as is: %v", volumeID, err)
			continue
		}
		if _, err := deviceAlias(nguid, devicePath); err != nil {
			klog.Warningf("pnfs: could not refresh the device alias for %s: %v", volumeID, err)
		}
	}
}

// KeepLayoutsPrimed probes every pNFS staging mount of this driver when it is
// first seen and after every reconnect to its server, until ctx ends. It
// also keeps every such mount's device alias current; see
// reconcileDeviceAliases.
func KeepLayoutsPrimed(ctx context.Context, driverName string) {
	primer := newLayoutPrimer(driverName)
	ticker := time.NewTicker(primeInterval)
	defer ticker.Stop()
	for {
		mounts, err := nfsclient.ReadMountstats()
		if err != nil {
			klog.Warningf("pnfs: reading the NFS mounts to keep layouts primed: %v", err)
		}
		reconcileDeviceAliases(ctx, primer, mounts)
		for _, path := range primer.due(mounts) {
			go func(path string) {
				// Blocks while the server is in its grace period, which is
				// why it runs beside the loop rather than in it.
				err := primeLayout(ctx, path)
				if err != nil {
					klog.Warningf("pnfs: probing %s for its layout: %v", path, err)
				} else {
					klog.V(2).Infof("pnfs: took the layout for %s", path)
				}
				primer.done(path, err)
			}(path)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// primedSizeFileName is where a staging mount's last prime is recorded, next
// to the volume context in the staging parent.
const primedSizeFileName = "pnfs-primed-size.json"

// primedSize is what a staging mount looked like when this node last took its
// layout: the size statfs reported, and the size the expansion asked for, zero
// for a prime at stage.
type primedSize struct {
	Size     int64 `json:"size"`
	Required int64 `json:"required"`
}

// stashPrimedSize records a prime of the mount at stagingPath.
func stashPrimedSize(stagingPath string, p primedSize) error {
	return stashContext(p, filepath.Dir(stagingPath), primedSizeFileName)
}

// lookupPrimedSize returns the last prime recorded for the mount at
// stagingPath, and false when there is none.
func lookupPrimedSize(stagingPath string) (*primedSize, bool) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(stagingPath), primedSizeFileName))
	if err != nil {
		return nil, false
	}
	var p primedSize
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, false
	}
	return &p, true
}

// recordPrime records a prime of the mount at stagingPath just taken, at the
// size the mount shows now, for an expansion to required bytes (zero at stage).
func recordPrime(stagingPath string, required int64) error {
	size, err := mountSize(stagingPath)
	if err != nil {
		return err
	}
	return stashPrimedSize(stagingPath, primedSize{Size: size, Required: required})
}
