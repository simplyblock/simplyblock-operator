//go:build !linux

package nvme

import (
	"fmt"

	"github.com/simplyblock/atlas/errs"
)

// identifyControllerMNAN is unavailable off Linux: the NVMe admin passthrough
// ioctl (NVME_IOCTL_ADMIN_CMD) is a Linux kernel interface.
func identifyControllerMNAN(devicePath string) (uint32, error) {
	return 0, fmt.Errorf("nvme identify controller: %w", errs.ErrUnsupported)
}

// identifyController is unavailable off Linux for the same reason.
func identifyController(devicePath string) ([]byte, error) {
	return nil, fmt.Errorf("nvme identify controller: %w", errs.ErrUnsupported)
}

// identifyNamespace is unavailable off Linux for the same reason.
func identifyNamespace(devicePath string, nsid NamespaceID) ([]byte, error) {
	return nil, fmt.Errorf("nvme identify namespace: %w", errs.ErrUnsupported)
}
