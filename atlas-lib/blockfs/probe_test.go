// Tests for the probe's decision procedure: the state it reaches for each kind
// of device, and above all that a device which will not answer a read is never
// reported as blank. The failing reads come from an injected opener, because a
// temporary file cannot be made to return EIO.
package blockfs

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// deviceWith returns a probe result for a device whose leading bytes are the
// given content.
func deviceWith(t *testing.T, content []byte) Result {
	t.Helper()

	device := filepath.Join(t.TempDir(), "device")
	if err := os.WriteFile(device, content, 0o600); err != nil {
		t.Fatalf("write device: %v", err)
	}
	return NewDeviceProber().Probe(context.Background(), device)
}

// withMagic returns probeLength bytes carrying magic at offset.
func withMagic(offset int, magic []byte) []byte {
	content := make([]byte, probeLength)
	copy(content[offset:], magic)
	return content
}

// ext4Device returns the bytes of a device holding an ext4 filesystem, as far
// as any probe is concerned: s_magic at 0x38 into a superblock at 1024.
func ext4Device() []byte {
	content := make([]byte, probeLength)
	binary.LittleEndian.PutUint16(content[1024+0x38:], 0xEF53)
	return content
}

func TestProbeClassifiesDeviceContent(t *testing.T) {
	tests := []struct {
		name          string
		content       []byte
		wantState     State
		wantSignature string
	}{
		{
			name:          "ext4",
			content:       ext4Device(),
			wantState:     StateFormatted,
			wantSignature: "ext",
		},
		{
			name:          "xfs",
			content:       withMagic(0, []byte("XFSB")),
			wantState:     StateFormatted,
			wantSignature: "xfs",
		},
		{
			name:          "btrfs",
			content:       withMagic(0x10040, []byte("_BHRfS_M")),
			wantState:     StateFormatted,
			wantSignature: "btrfs",
		},
		{
			name:          "an LUKS container is data even though it cannot be mounted",
			content:       withMagic(0, []byte{'L', 'U', 'K', 'S', 0xBA, 0xBE}),
			wantState:     StateForeign,
			wantSignature: "LUKS",
		},
		{
			name:          "an LVM physical volume label in the second sector",
			content:       withMagic(512, []byte("LABELONE")),
			wantState:     StateForeign,
			wantSignature: "LVM2",
		},
		{
			name:          "a partition table",
			content:       withMagic(512, []byte("EFI PART")),
			wantState:     StateForeign,
			wantSignature: "GPT",
		},
		{
			name:      "a freshly provisioned volume reads as zeros",
			content:   make([]byte, probeLength),
			wantState: StateBlank,
		},
		{
			name:      "content with no signature is not blank either",
			content:   withMagic(8192, []byte("some tenant's bytes")),
			wantState: StateUnknown,
		},
		{
			name:      "a device shorter than the probe window is still classified",
			content:   ext4Device()[:4096],
			wantState: StateFormatted,
		},
		{
			name:      "a short blank device is blank",
			content:   make([]byte, 4096),
			wantState: StateBlank,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := deviceWith(t, tc.content)
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q (err %v)", got.State, tc.wantState, got.Err)
			}
			if tc.wantSignature != "" && got.Signature != tc.wantSignature {
				t.Errorf("signature = %q, want %q", got.Signature, tc.wantSignature)
			}
		})
	}
}

// erroringDevice fails every read with err, the way a block device does once
// its NVMe-oF paths are gone. Close is signaled over a channel because it runs
// on the probe's read goroutine, not the caller's.
type erroringDevice struct {
	err    error
	closed chan struct{}
}

func newErroringDevice(err error) *erroringDevice {
	return &erroringDevice{err: err, closed: make(chan struct{})}
}

func (d *erroringDevice) ReadAt([]byte, int64) (int, error) { return 0, d.err }

func (d *erroringDevice) Close() error {
	close(d.closed)
	return nil
}

// TestProbeReportsUnreadableRatherThanBlank is the property the package exists
// for: a device whose reads fail must never come back as blank, because a
// caller reading "blank" formats it.
func TestProbeReportsUnreadableRatherThanBlank(t *testing.T) {
	device := newErroringDevice(syscall.EIO)
	prober := NewDeviceProber(WithOpener(func(string) (ReaderAtCloser, error) {
		return device, nil
	}))

	got := prober.Probe(context.Background(), "/dev/nvme0n1")
	if got.State != StateUnreadable {
		t.Fatalf("state = %q, want %q", got.State, StateUnreadable)
	}
	if !errors.Is(got.Err, syscall.EIO) {
		t.Errorf("err = %v, want it to wrap EIO", got.Err)
	}
}

// TestProbeReportsUnreadableWhenTheDeviceCannotBeOpened covers the device that
// vanished between the connect and the stage: total path loss removes the node.
func TestProbeReportsUnreadableWhenTheDeviceCannotBeOpened(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nvme0n1")

	got := NewDeviceProber().Probe(context.Background(), missing)
	if got.State != StateUnreadable {
		t.Fatalf("state = %q, want %q", got.State, StateUnreadable)
	}
	if !errors.Is(got.Err, os.ErrNotExist) {
		t.Errorf("err = %v, want it to wrap ErrNotExist", got.Err)
	}
}

// TestProbeReportsUnreadableWhenTheDeviceReturnsNothing covers a namespace the
// kernel published with no size behind it, which reads as an immediate EOF.
func TestProbeReportsUnreadableWhenTheDeviceReturnsNothing(t *testing.T) {
	got := deviceWith(t, nil)
	if got.State != StateUnreadable {
		t.Fatalf("state = %q, want %q", got.State, StateUnreadable)
	}
}

// blockingDevice never answers, the way a read against a stalled path does.
type blockingDevice struct{ release chan struct{} }

func (d *blockingDevice) ReadAt([]byte, int64) (int, error) {
	<-d.release
	return 0, syscall.EIO
}
func (d *blockingDevice) Close() error { return nil }

// TestProbeTimesOutRatherThanBlocking pins the bound on a probe: a read that
// never returns must not hold a NodeStageVolume open indefinitely, and the
// result is still unreadable rather than blank.
func TestProbeTimesOutRatherThanBlocking(t *testing.T) {
	device := &blockingDevice{release: make(chan struct{})}
	// Released at the end so the abandoned read goroutine finishes with the
	// test rather than outliving it.
	defer close(device.release)

	prober := NewDeviceProber(
		WithOpener(func(string) (ReaderAtCloser, error) { return device, nil }),
		WithTimeout(50*time.Millisecond),
	)

	start := time.Now()
	got := prober.Probe(context.Background(), "/dev/nvme0n1")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("probe took %s; it should give up after its timeout", elapsed)
	}
	if got.State != StateUnreadable {
		t.Fatalf("state = %q, want %q", got.State, StateUnreadable)
	}
	if !errors.Is(got.Err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap DeadlineExceeded", got.Err)
	}
}

// TestProbeHonorsCallerCancellation keeps the probe from outliving the RPC that
// asked for it.
func TestProbeHonorsCallerCancellation(t *testing.T) {
	device := &blockingDevice{release: make(chan struct{})}
	defer close(device.release)

	prober := NewDeviceProber(WithOpener(func(string) (ReaderAtCloser, error) { return device, nil }))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := prober.Probe(ctx, "/dev/nvme0n1")
	if got.State != StateUnreadable {
		t.Fatalf("state = %q, want %q", got.State, StateUnreadable)
	}
}

// partialDevice returns some bytes and then fails, the way a device does when
// the path dies mid-probe.
type partialDevice struct{ data []byte }

func (d *partialDevice) ReadAt(p []byte, _ int64) (int, error) {
	n := copy(p, d.data)
	return n, syscall.EIO
}
func (d *partialDevice) Close() error { return nil }

// TestProbeTrustsASignatureFoundInAPartialRead resolves a half-answered read
// toward keeping the data: a filesystem seen in the bytes that did arrive is
// there whatever happened to the rest.
func TestProbeTrustsASignatureFoundInAPartialRead(t *testing.T) {
	prober := NewDeviceProber(WithOpener(func(string) (ReaderAtCloser, error) {
		return &partialDevice{data: ext4Device()[:2048]}, nil
	}))

	got := prober.Probe(context.Background(), "/dev/nvme0n1")
	if got.State != StateFormatted {
		t.Fatalf("state = %q, want %q", got.State, StateFormatted)
	}
}

// TestProbeDoesNotConcludeBlankFromAPartialRead is the same situation without
// the signature: zeros that arrived before the read failed say nothing about
// the bytes that did not.
func TestProbeDoesNotConcludeBlankFromAPartialRead(t *testing.T) {
	prober := NewDeviceProber(WithOpener(func(string) (ReaderAtCloser, error) {
		return &partialDevice{data: make([]byte, 2048)}, nil
	}))

	got := prober.Probe(context.Background(), "/dev/nvme0n1")
	if got.State != StateUnreadable {
		t.Fatalf("state = %q, want %q", got.State, StateUnreadable)
	}
}

// TestProbeClosesTheDevice keeps a probe per stage from leaking a descriptor.
func TestProbeClosesTheDevice(t *testing.T) {
	device := newErroringDevice(io.EOF)
	prober := NewDeviceProber(WithOpener(func(string) (ReaderAtCloser, error) { return device, nil }))

	prober.Probe(context.Background(), "/dev/nvme0n1")

	// The read runs on its own goroutine, so the close it defers may land just
	// after Probe returns.
	select {
	case <-device.closed:
	case <-time.After(10 * time.Second):
		t.Error("probe did not close the device")
	}
}
