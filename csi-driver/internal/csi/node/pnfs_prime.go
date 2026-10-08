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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/klog"

	"github.com/simplyblock/atlas/nfsclient"
	export "github.com/simplyblock/atlas/nfsexport"
)

// scsiLayout is the layout type mountstats reports for a pNFS SCSI mount.
const scsiLayout = "LAYOUT_SCSI"

// primeInterval is how often the mounts are checked for a reconnect. The
// server holds a reconnected client's layouts for the length of its grace
// period and a while after, so seconds are enough to be first.
const primeInterval = 2 * time.Second

// primeLayout triggers a LAYOUTGET from this process rather than a pod. See the
// file comment.
func primeLayout(ctx context.Context, stagingPath string) error {
	// Checked before rather than during: the syscalls below are not
	// cancellable, and a stage that gave up should not add I/O.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("pnfs: not taking the layout for %s: %w", stagingPath, err)
	}

	probe := filepath.Join(stagingPath, export.LayoutProbeName)
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
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

// KeepLayoutsPrimed probes every pNFS staging mount of this driver when it is
// first seen and after every reconnect to its server, until ctx ends.
func KeepLayoutsPrimed(ctx context.Context, driverName string) {
	primer := newLayoutPrimer(driverName)
	ticker := time.NewTicker(primeInterval)
	defer ticker.Stop()
	for {
		mounts, err := nfsclient.ReadMountstats()
		if err != nil {
			klog.Warningf("pnfs: reading the NFS mounts to keep layouts primed: %v", err)
		}
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
