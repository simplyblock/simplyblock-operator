// Tests for the metadata server's reservation key: its layout, its separation
// from nfsd's client keys, and its bounds (test-plan-pnfs-mds-standby.md U-01
// to U-04).
package nfsexport

import (
	"errors"
	"testing"
)

func TestMDSReservationKeyLayout(t *testing.T) {
	// sha256("abc") starts ba7816, so the tag is 0xba7816.
	key, err := MDSReservationKey("abc", 5)
	if err != nil {
		t.Fatalf("MDSReservationKey: %v", err)
	}
	if want := uint64(0x01ba781600000005); key != want {
		t.Fatalf("key = %#016x, want %#016x", key, want)
	}
}

func TestMDSReservationKeySeparatesStatefulSetsAndEpochs(t *testing.T) {
	a1, _ := MDSReservationKey("uid-a", 1)
	b1, _ := MDSReservationKey("uid-b", 1)
	a2, _ := MDSReservationKey("uid-a", 2)
	if a1 == b1 {
		t.Errorf("two StatefulSets at the same epoch share key %#016x", a1)
	}
	if a1 == a2 {
		t.Errorf("one StatefulSet at two epochs shares key %#016x", a1)
	}
}

// A client key is (cl_boot << 32) | cl_id, so its top byte is the top byte of
// the boot time. Boot times with top byte 0x01 run from 1970-07-14 to
// 1971-01-24, and no server booted in 2000 or later has one.
func TestNoClientKeyIsAnMDSKey(t *testing.T) {
	const year2000 = uint64(946684800)
	for boot := year2000; boot <= 0xffffffff; boot += 1 << 24 {
		clientKey := boot << 32
		if IsMDSKey(clientKey) {
			t.Fatalf("client key of boot %d (%#016x) reads as an MDS key", boot, clientKey)
		}
	}
	if IsMDSKey(0xffffffff_ffffffff) {
		t.Error("the largest possible client key reads as an MDS key")
	}
}

func TestIsMDSKeyCoversTheRangeAndNFSDsConstant(t *testing.T) {
	for _, key := range []uint64{0x0100000000000000, 0x01ffffffffffffff, 0x01ba781600000005} {
		if !IsMDSKey(key) {
			t.Errorf("IsMDSKey(%#016x) = false, want true", key)
		}
	}
	for _, key := range []uint64{0, 0x00ffffffffffffff, 0x0200000000000000} {
		if IsMDSKey(key) {
			t.Errorf("IsMDSKey(%#016x) = true, want false", key)
		}
	}
}

func TestMDSReservationKeyEpochBounds(t *testing.T) {
	for _, epoch := range []uint64{0, 1 << 32, 1<<32 + 1, ^uint64(0)} {
		if _, err := MDSReservationKey("abc", epoch); !errors.Is(err, ErrInvalidEpoch) {
			t.Errorf("epoch %d: err = %v, want ErrInvalidEpoch", epoch, err)
		}
	}
	key, err := MDSReservationKey("abc", 1<<32-1)
	if err != nil {
		t.Fatalf("largest epoch refused: %v", err)
	}
	if want := uint64(0x01ba7816ffffffff); key != want {
		t.Errorf("largest epoch key = %#016x, want %#016x", key, want)
	}
}

func TestParseMDSKeyRoundTrips(t *testing.T) {
	key, _ := MDSReservationKey("abc", 7)
	tag, epoch, ok := ParseMDSKey(key)
	if !ok || tag != 0xba7816 || epoch != 7 {
		t.Fatalf("ParseMDSKey(%#016x) = %#x, %d, %v", key, tag, epoch, ok)
	}
	if _, _, ok := ParseMDSKey(0x6ad8d693350aea19); ok {
		t.Error("a client key parsed as an MDS key")
	}
	for _, key := range []uint64{0x0100000000000000, 0x01ba781600000000} {
		if _, _, ok := ParseMDSKey(key); ok {
			t.Errorf("ParseMDSKey(%#016x) = ok, but no promotion makes a key with epoch 0", key)
		}
	}
}
