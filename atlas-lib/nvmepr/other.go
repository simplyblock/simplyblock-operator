//go:build !linux

package nvmepr

import (
	"fmt"

	"github.com/simplyblock/atlas/errs"
)

// Open is unavailable off Linux: the persistent-reservation ioctls and the NVMe
// passthrough are Linux kernel interfaces.
func Open(path string) (Device, error) {
	return nil, fmt.Errorf("nvmepr open %s: %w", path, errs.ErrUnsupported)
}
