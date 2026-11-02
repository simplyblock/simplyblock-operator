// Asking the kernel to look again for namespaces on a connected subsystem.
// A namespace added to a subsystem a host is already connected to is normally
// announced by the target, and this is the fallback for when the announcement
// has not arrived: the same request `nvme ns-rescan` makes, written to each live
// controller's rescan_controller attribute so the node needs no nvme-cli for it.

package swaprpc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/simplyblock/atlas/nvme"
)

// SysfsRescan returns a Config.Rescan over the host's sysfs.
func SysfsRescan(subs nvme.SubsystemResolver) func(ctx context.Context, nqn string) error {
	return func(ctx context.Context, nqn string) error {
		sub, err := subs.ByNQN(ctx, nqn)
		if err != nil {
			return fmt.Errorf("rescan %s: %w", nqn, err)
		}
		asked := 0
		for _, c := range sub.Controllers {
			if c.State != "live" || c.SysfsPath == "" {
				continue
			}
			attr := filepath.Join(c.SysfsPath, "rescan_controller")
			if err := os.WriteFile(attr, []byte("1"), 0o200); err != nil {
				return fmt.Errorf("rescan %s through %s: %w", nqn, c.ID, err)
			}
			asked++
		}
		if asked == 0 {
			return fmt.Errorf("rescan %s: no live controller", nqn)
		}
		return nil
	}
}
