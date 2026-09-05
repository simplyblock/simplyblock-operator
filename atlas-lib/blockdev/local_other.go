//go:build !linux

// The local device reader on platforms this product does not run on.
//
// Reading a block device the way the probe needs it read is Linux-specific: the
// cache-bypassing open, the block-size ioctls, and the device numbers all are.
// The package still builds and its tests still run elsewhere, because a
// developer's machine is where the readings are exercised against captured
// images, and those need no device at all.

package blockdev

import (
	"context"
	"errors"
	"runtime"
)

// errNotLinux is what the local device paths answer off Linux. It is an error
// rather than a degraded reading, so nothing here can be mistaken for a device
// that was read and found empty.
var errNotLinux = errors.New("blockdev: reading a local block device is supported on Linux only, and this is " + runtime.GOOS)

// NewProber returns a Prober whose reads report that this platform has no local
// block devices to read.
func NewProber(opts ...Option) *Prober { return NewProberWithOpener(OpenLocal, opts...) }

// OpenLocal reports that a local device cannot be opened on this platform.
func OpenLocal(context.Context, Device) (Reader, error) { return nil, errNotLinux }

// ResolveDevice reports that a local device cannot be inspected on this platform.
func ResolveDevice(string) (Device, error) { return Device{}, errNotLinux }
