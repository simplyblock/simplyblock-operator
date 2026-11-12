package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// exitError is a failed command with an exit status, as exec.ExitError is.
type exitError int

func (e exitError) Error() string { return "exit status " + string(rune('0'+int(e))) }
func (e exitError) ExitCode() int { return int(e) }

// guest is a fake guest: the commands it ran, whether its state is mounted, and
// what blkid finds on the state disk.
type guest struct {
	ran     []string
	mounted bool
	blkid   func() ([]byte, error)
}

func (g *guest) run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	g.ran = append(g.ran, cmd)
	switch name {
	case "blkid":
		return g.blkid()
	case "mount":
		g.mounted = true
	}
	return nil, nil
}

func (g *guest) ranAny(prefix string) bool {
	return slices.ContainsFunc(g.ran, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func activation(t *testing.T, g *guest) Activation {
	t.Helper()
	return Activation{
		StateDevice: "/dev/disk/by-id/virtio-pnfs-state",
		StateDir:    filepath.Join(t.TempDir(), "var", "lib", "nfs"),
		GateFile:    filepath.Join(t.TempDir(), "mds", "active"),
		Tmpfiles:    "/usr/lib/tmpfiles.d/pnfs.conf",
		Mounted:     func(string) (bool, error) { return g.mounted, nil },
		Run:         g.run,
	}
}

// Regression: 2026-11-11-standby-guest-never-activated. The guest image moved
// to a warm-standby boot: no state mount, and nfsd gated on /run/mds/active,
// which only a promotion creates. Nothing on this operator promotes, so every
// metadata server built from the moving pnfsos tag stayed unhealthy with "the
// state disk is not mounted" until its boot deadline restarted the pod, and
// pNFS was down on every cluster that picked the image up.
func TestAGuestBootedAsAStandbyIsActivated(t *testing.T) {
	g := &guest{blkid: func() ([]byte, error) { return []byte("ext4\n"), nil }}
	a := activation(t, g)

	if err := a.Activate(context.Background()); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !g.mounted {
		t.Error("the state disk was not mounted")
	}
	if _, err := os.Stat(a.GateFile); err != nil {
		t.Errorf("the gate file was not created: %v", err)
	}
	if !g.ranAny("systemctl start nfs-server.service") {
		t.Errorf("nfsd was not started; ran %q", g.ran)
	}
	if g.ranAny("mkfs") {
		t.Error("a state disk carrying a filesystem was formatted")
	}
}

// A blank state disk, as on a first boot, is formatted before it is mounted.
func TestABlankStateDiskIsFormattedFirst(t *testing.T) {
	g := &guest{blkid: func() ([]byte, error) { return nil, exitError(2) }}
	if err := activation(t, g).Activate(context.Background()); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	mkfs := slices.IndexFunc(g.ran, func(c string) bool { return strings.HasPrefix(c, "mkfs.ext4") })
	mount := slices.IndexFunc(g.ran, func(c string) bool { return strings.HasPrefix(c, "mount") })
	if mkfs < 0 || mount < mkfs {
		t.Errorf("want mkfs.ext4 before mount; ran %q", g.ran)
	}
}

// blkid failing to read the disk says nothing about whether it holds a
// filesystem. Formatting then would destroy the client-recovery database, so
// activation fails and is retried instead.
func TestAnUnreadableStateDiskIsNeverFormatted(t *testing.T) {
	g := &guest{blkid: func() ([]byte, error) { return nil, exitError(4) }}
	if err := activation(t, g).Activate(context.Background()); err == nil {
		t.Error("Activate succeeded on a state disk blkid could not read")
	}
	if g.ranAny("mkfs") {
		t.Errorf("an unreadable state disk was formatted; ran %q", g.ran)
	}
}

// An image that mounts the state disk and starts nfsd at boot is left as it is:
// nothing is mounted again, and starting running units changes nothing.
func TestAGuestAlreadyServingIsLeftAsItIs(t *testing.T) {
	g := &guest{mounted: true, blkid: func() ([]byte, error) { return nil, errors.New("not called") }}
	if err := activation(t, g).Activate(context.Background()); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if g.ranAny("blkid") || g.ranAny("mount") || g.ranAny("mkfs") {
		t.Errorf("a mounted state disk was touched; ran %q", g.ran)
	}
}
