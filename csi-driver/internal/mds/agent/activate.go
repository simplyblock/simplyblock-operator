// Making the guest the active metadata server at boot, when it is the only one.
//
// A guest image built for the warm standby boots without its state mounted and
// without nfsd, which wait for a gate file the agent creates on promotion. With
// no standby there is nothing to promote it, so the agent activates the guest
// itself. On an image that still mounts the state disk and starts nfsd at boot,
// every step finds its work done.

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The units started, in this order: nfsd reads the client-recovery database
// through nfsdcld the moment its threads start.
var activationUnits = []string{"nfsdcld.service", "nfs-mountd.service", "nfs-server.service"}

// blkidNothingFound is blkid's exit status for a device with no filesystem.
const blkidNothingFound = 2

// Command runs one command and returns its combined output. An error that has
// an ExitCode method reports the command's exit status.
type Command func(ctx context.Context, name string, args ...string) ([]byte, error)

// Activation is what activating the guest touches.
type Activation struct {
	// StateDevice is the state disk, StateDir where it is mounted.
	StateDevice string
	StateDir    string
	// GateFile lets the guest's nfsd units start.
	GateFile string
	// Tmpfiles recreates what the state mount hides, when the image has it.
	Tmpfiles string
	Mounted  func(dir string) (bool, error)
	Run      Command
	Log      Logger
}

// Activate makes the guest serve: the state disk mounted, the gate open, and
// nfsd started.
func (a Activation) Activate(ctx context.Context) error {
	if err := a.mountState(ctx); err != nil {
		return err
	}
	if a.Tmpfiles != "" {
		if _, err := os.Stat(a.Tmpfiles); err == nil {
			if err := a.run(ctx, "systemd-tmpfiles", "--create", a.Tmpfiles); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(a.GateFile), 0o755); err != nil {
		return fmt.Errorf("creating the gate file's directory: %w", err)
	}
	if err := os.WriteFile(a.GateFile, nil, 0o644); err != nil {
		return fmt.Errorf("creating the gate file: %w", err)
	}
	for _, unit := range activationUnits {
		if err := a.run(ctx, "systemctl", "start", unit); err != nil {
			return err
		}
	}
	return nil
}

// mountState mounts the state disk unless it is mounted already, formatting it
// only when blkid finds no filesystem on it. A blkid that fails otherwise has
// not read the disk, and formatting then would destroy the client-recovery
// database.
func (a Activation) mountState(ctx context.Context) error {
	mounted, err := a.Mounted(a.StateDir)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", a.StateDir, err)
	}
	if mounted {
		return nil
	}
	out, err := a.Run(ctx, "blkid", "-p", "-o", "value", "-s", "TYPE", a.StateDevice)
	var exit interface{ ExitCode() int }
	switch {
	case err == nil && strings.TrimSpace(string(out)) != "":
	case err != nil && errors.As(err, &exit) && exit.ExitCode() == blkidNothingFound:
		if err := a.run(ctx, "mkfs.ext4", "-q", a.StateDevice); err != nil {
			return err
		}
	default:
		return fmt.Errorf("reading the state disk %s: %w (%s)", a.StateDevice, err, strings.TrimSpace(string(out)))
	}
	if err := os.MkdirAll(a.StateDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", a.StateDir, err)
	}
	return a.run(ctx, "mount", "-t", "ext4", "-o", "noatime", a.StateDevice, a.StateDir)
}

func (a Activation) run(ctx context.Context, name string, args ...string) error {
	if out, err := a.Run(ctx, name, args...); err != nil {
		return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ActivateUntilDone retries Activate until it succeeds or ctx ends.
func (a Activation) ActivateUntilDone(ctx context.Context, interval time.Duration) {
	for {
		err := a.Activate(ctx)
		if err == nil {
			if a.Log != nil {
				a.Log.Infof("the guest is active: state disk mounted at %s, nfsd started", a.StateDir)
			}
			return
		}
		if a.Log != nil {
			a.Log.Warningf("activating the guest: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
