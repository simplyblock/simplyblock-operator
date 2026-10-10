// Priming a pNFS client's device lookup: which staging mounts need a probe.

package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/simplyblock/atlas/nfsclient"
	export "github.com/simplyblock/atlas/nfsexport"
)

const (
	primeDriver  = "csi.simplyblock.io"
	stagingMount = "/var/lib/kubelet/plugins/kubernetes.io/csi/csi.simplyblock.io/9d88/globalmount/c:p:6e17"
)

var errProbeForTest = errors.New("probe failed")

func pnfsMount(path string, connects uint64) nfsclient.Mount {
	return nfsclient.Mount{
		MountPoint: path, FSType: "nfs4", LayoutTypes: []string{"LAYOUT_SCSI"}, Connects: connects,
	}
}

// A staging mount is probed when first seen: this process may have restarted
// while the server did, and a probe that finds the device cached costs a write.
func TestAStagingMountIsPrimedWhenFirstSeen(t *testing.T) {
	p := newLayoutPrimer(primeDriver)
	if got := p.due([]nfsclient.Mount{pnfsMount(stagingMount, 6)}); !slices.Equal(got, []string{stagingMount}) {
		t.Errorf("due = %q, want the staging mount", got)
	}
}

// A reconnect is how a server restart shows on the client, and the restart
// drops the client's cached devices.
func TestAStagingMountIsPrimedAgainAfterAReconnect(t *testing.T) {
	p := newLayoutPrimer(primeDriver)
	p.done(p.due([]nfsclient.Mount{pnfsMount(stagingMount, 6)})[0], nil)

	if got := p.due([]nfsclient.Mount{pnfsMount(stagingMount, 6)}); len(got) != 0 {
		t.Errorf("due = %q without a reconnect, want nothing", got)
	}
	if got := p.due([]nfsclient.Mount{pnfsMount(stagingMount, 7)}); !slices.Equal(got, []string{stagingMount}) {
		t.Errorf("due = %q after a reconnect, want the staging mount", got)
	}
}

// A probe blocks for as long as the server's grace period, so a second one is
// not started beside it.
func TestAProbeStillRunningIsNotStartedTwice(t *testing.T) {
	p := newLayoutPrimer(primeDriver)
	p.due([]nfsclient.Mount{pnfsMount(stagingMount, 6)})
	if got := p.due([]nfsclient.Mount{pnfsMount(stagingMount, 7)}); len(got) != 0 {
		t.Errorf("due = %q while the probe runs, want nothing", got)
	}
}

// A probe that failed is retried on the next pass, even without a reconnect.
func TestAFailedProbeIsRetried(t *testing.T) {
	p := newLayoutPrimer(primeDriver)
	p.done(p.due([]nfsclient.Mount{pnfsMount(stagingMount, 6)})[0], errProbeForTest)
	if got := p.due([]nfsclient.Mount{pnfsMount(stagingMount, 6)}); !slices.Equal(got, []string{stagingMount}) {
		t.Errorf("due = %q after a failed probe, want the staging mount", got)
	}
}

// Only this driver's pNFS staging mounts are probed: a pod's bind of one shares
// its client, another driver's mount is not ours to write to, and a mount with
// no layout has no device to resolve.
func TestOnlyThisDriversPNFSStagingMountsArePrimed(t *testing.T) {
	plain := pnfsMount("/var/lib/kubelet/plugins/kubernetes.io/csi/csi.simplyblock.io/aa/globalmount/x", 1)
	plain.LayoutTypes = nil
	mounts := []nfsclient.Mount{
		pnfsMount("/var/lib/kubelet/pods/1e98/volumes/kubernetes.io~csi/pvc-f117/mount", 1),
		pnfsMount("/var/lib/kubelet/plugins/kubernetes.io/csi/nfs.csi.k8s.io/bb/globalmount", 1),
		plain,
	}
	if got := newLayoutPrimer(primeDriver).due(mounts); len(got) != 0 {
		t.Errorf("due = %q, want nothing", got)
	}
}

// primeLayout is what makes the block layout usable from a pod at all.
//
// The client resolves the layout's device by opening
// /dev/disk/by-id/nvme-eui.<nguid>, and it resolves that path in the mount
// namespace of whichever process triggered the I/O. A pod's /dev is the minimal
// one kubelet builds, with no disk/ in it, so a layout first requested by the
// application can never resolve, and the failure is expensive: the device is
// marked unavailable for two minutes and the layout's read-write fail bit is
// set, so everything afterward bypasses pNFS and routes through the metadata
// server.
//
// The node plugin's own container mounts the host's /dev. Touching the mount
// here, before any pod does, puts the resolution in a namespace where it
// succeeds and leaves the device cached for every later reader.
func TestPrimeLayoutTouchesTheMountAndLeavesNothingBehind(t *testing.T) {
	staging := t.TempDir()

	if err := primeLayout(context.Background(), staging); err != nil {
		t.Fatalf("primeLayout: %v", err)
	}

	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatalf("reading the staging path: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("priming left %v behind; the export is the user's filesystem", names)
	}
}

// A staging path that cannot be written is reported rather than hidden, but it
// is the caller that decides what to do about it.
func TestPrimeLayoutReportsAnUnwritableMount(t *testing.T) {
	if err := primeLayout(context.Background(), "/nonexistent/staging/path"); err == nil {
		t.Error("priming an unwritable mount reported success")
	}
}

// A stage that has already given up does not start I/O on the mount it gave up
// on. The file operations are plain syscalls and cannot be interrupted once
// begun, so the only useful place to look at the context is before them.
func TestPrimeLayoutRespectsACanceledStage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	staging := t.TempDir()
	if err := primeLayout(ctx, staging); err == nil {
		t.Fatal("priming ran for a stage that had already been canceled")
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatalf("reading the staging path: %v", err)
	}
	if len(entries) != 0 {
		t.Error("priming touched the mount despite the cancellation")
	}
}

// openProbeRecording replaces how the probe is opened for one test, recording
// each open's flags and holding it until release is closed.
func openProbeRecording(t *testing.T, release <-chan struct{}) (opened chan int) {
	t.Helper()
	opened = make(chan int, 8)
	real := openProbe
	openProbe = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		opened <- flag
		<-release
		return real(name, flag, perm)
	}
	t.Cleanup(func() { openProbe = real })
	return opened
}

// Regression: 2026-10-10-pnfs-probe-truncate-self-recall (pnfs-1791629269). A
// truncating open of a probe file this client already held a layout on sent a
// SETATTR to size 0. The server recalls every layout on a size change, this
// client's own included, and answered NFS4ERR_DELAY until it came back, which
// it never did while the truncate held the inode. So the probe must never
// change the file's size. A probe another node left behind, larger than the
// one block written here, is held open across the probe and keeps its size.
func TestPrimeLayoutNeverChangesAnExistingProbesSize(t *testing.T) {
	staging := t.TempDir()
	const leftBehind = 8192
	held, err := os.OpenFile(filepath.Join(staging, export.LayoutProbeName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("creating the probe another node left behind: %v", err)
	}
	defer func() { _ = held.Close() }()
	if _, err := held.Write(make([]byte, leftBehind)); err != nil {
		t.Fatalf("writing it: %v", err)
	}

	if err := primeLayout(context.Background(), staging); err != nil {
		t.Fatalf("primeLayout: %v", err)
	}

	info, err := held.Stat()
	if err != nil {
		t.Fatalf("stat through the held handle: %v", err)
	}
	if info.Size() != leftBehind {
		t.Errorf("the probe's size went from %d to %d: priming resized the file", leftBehind, info.Size())
	}
}

// Regression: 2026-10-10-pnfs-probe-truncate-self-recall (pnfs-1791629269). A
// stage's probe and the background loop's probed one mount at once: one held
// the inode while the other's layout had to be returned. Probes of one mount
// run one after the other.
func TestProbesOfOneMountDoNotOverlap(t *testing.T) {
	release := make(chan struct{})
	opened := openProbeRecording(t, release)
	staging := t.TempDir()

	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- primeLayout(context.Background(), staging) }()
	}
	<-opened
	select {
	case <-opened:
		t.Error("a second probe of the same mount started while the first was running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("primeLayout: %v", err)
		}
	}
}

// A stage that gives up while its probe waits behind another probe of the same
// mount starts no I/O once the first is done.
func TestAProbeCanceledWhileWaitingDoesNotStart(t *testing.T) {
	release := make(chan struct{})
	opened := openProbeRecording(t, release)
	staging := t.TempDir()

	first := make(chan error, 1)
	go func() { first <- primeLayout(context.Background(), staging) }()
	<-opened

	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { second <- primeLayout(ctx, staging) }()
	// The second probe is queued on the mount's lock by now; nothing it does
	// before the lock can be observed from here.
	time.Sleep(100 * time.Millisecond)
	cancel()
	close(release)

	if err := <-first; err != nil {
		t.Errorf("first primeLayout: %v", err)
	}
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Errorf("second primeLayout = %v, want context.Canceled", err)
	}
	select {
	case <-opened:
		t.Error("the canceled probe opened the probe file after the first finished")
	default:
	}
}

// Different mounts are different servers' state, and one slow probe must not
// hold up another mount's stage.
func TestProbesOfDifferentMountsRunTogether(t *testing.T) {
	release := make(chan struct{})
	opened := openProbeRecording(t, release)

	errs := make(chan error, 2)
	for _, staging := range []string{t.TempDir(), t.TempDir()} {
		go func() { errs <- primeLayout(context.Background(), staging) }()
	}
	for range 2 {
		select {
		case <-opened:
		case <-time.After(2 * time.Second):
			t.Fatal("a probe of one mount waited for a probe of another")
		}
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("primeLayout: %v", err)
		}
	}
}
