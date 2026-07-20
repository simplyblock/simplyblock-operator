//go:build !linux

package nvme

import (
	"fmt"

	"github.com/simplyblock/atlas/errs"
)

// identifyControllerNN is unavailable off Linux: the NVMe admin passthrough
// ioctl (NVME_IOCTL_ADMIN_CMD) is a Linux kernel interface.
func identifyControllerNN(devicePath string) (uint32, error) {
	return 0, fmt.Errorf("nvme identify controller: %w", errs.ErrUnsupported)
}
