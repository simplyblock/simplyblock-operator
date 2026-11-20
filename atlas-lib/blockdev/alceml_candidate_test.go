// What a caller is told about a device this product already took.
//
// The reading is half the answer and the candidate is the other half, because a
// discovery run acts on whether the device was offered rather than on what the
// bytes were called.

package blockdev

import (
	"context"
	"testing"
)

// TestADeviceThisProductWroteIsOffered covers the disks a re-install finds.
//
// Regression: 2026-09-20-alceml-superblock-read-as-unknown-bytes — a fleet that
// had held a simplyblock cluster reported having no disks at all. Every device
// carried an alceml superblock, which blkid and wipefs both read as nothing, so
// the reading called it bytes matching no known signature and the candidate was
// refused NotBlank. The run meant to show a fleet what it has said it had
// nothing, and nothing distinguished that from a fleet whose disks genuinely
// belong to something else.
func TestADeviceThisProductWroteIsOffered(t *testing.T) {
	const size = 6251233968 * 512
	taken := blank(size)
	taken.bytes[0] = []byte("ALCEML_STORAGE\x00\x00")

	in := inspectorOver(t, map[string]sparse{"nvme0n1": taken}, nil)

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	got := found(t, cands, "nvme0n1")
	if got.RejectedFor(ReasonNotBlank) {
		t.Error("a device this product wrote is refused as carrying an unknown format")
	}
	if !got.Available() {
		t.Errorf("a device no storage node holds was not offered: %v", got.Rejections)
	}
	if got.Reading.Content != ContentSimplyblock {
		t.Errorf("it reads as %v, want %v", got.Reading.Content, ContentSimplyblock)
	}
}

// A disk this product wrote and something is still using is refused for the
// use, which is the reason that matters: the superblock says whose it was and
// the usage says whether it is free now.
func TestADeviceThisProductWroteIsStillRefusedWhileHeld(t *testing.T) {
	const size = 6251233968 * 512
	taken := blank(size)
	taken.bytes[0] = []byte("ALCEML_STORAGE\x00\x00")

	held := func(path string) error {
		if path == "/dev/nvme0n1" {
			return ErrDeviceBusy
		}
		return nil
	}
	in := inspectorOver(t, map[string]sparse{"nvme0n1": taken}, held)

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	got := found(t, cands, "nvme0n1")
	if got.Available() {
		t.Error("a device something is still using was offered")
	}
}
