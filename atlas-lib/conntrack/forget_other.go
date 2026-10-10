//go:build !linux

// Forget off Linux: there is no conntrack table to reach.

package conntrack

import (
	"fmt"

	"github.com/simplyblock/atlas/errs"
)

func forget(Selector) (uint, error) {
	return 0, fmt.Errorf("conntrack: %w off Linux", errs.ErrUnsupported)
}
