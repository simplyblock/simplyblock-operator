// The reservation key a metadata server instance registers under, and the
// parse the guest agent applies to a key it reads off a namespace. It lives
// here, beside the export it fences, because the operator computes the key for
// a promotion and the agent checks it, and two copies of the layout would
// drift (design-pnfs-mds-standby.md §6.2).
package nfsexport

import (
	"crypto/sha256"
	"errors"
	"fmt"
)

const (
	// mdsKeyTopByte is the top byte of every metadata server key. nfsd's own
	// constant key, 0x0100000000000000, has it, and so does every key this file
	// derives. A client key is (cl_boot << 32) | cl_id with cl_boot the
	// server's boot time in seconds, so its top byte is 0x01 only for a boot
	// between July 1970 and January 1971.
	mdsKeyTopByte = 0x01

	// maxMDSEpoch is the largest epoch that fits the key's low 32 bits.
	maxMDSEpoch = 1<<32 - 1
)

// ErrInvalidEpoch is an epoch outside 1 to 2^32-1.
var ErrInvalidEpoch = errors.New("invalid mds epoch")

// MDSReservationKey returns the key a metadata server instance registers
// under for one promotion: the top byte 0x01, then 24 bits taken from the
// SHA-256 of the StatefulSet's UID, then the epoch. The tag keeps a recreated
// StatefulSet, whose epoch restarts at 1, from reusing the keys of a guest of
// the old one that still runs on a lost node.
func MDSReservationKey(statefulSetUID string, epoch uint64) (uint64, error) {
	if epoch < 1 || epoch > maxMDSEpoch {
		return 0, fmt.Errorf("%w: %d", ErrInvalidEpoch, epoch)
	}
	return uint64(mdsKeyTopByte)<<56 | uint64(mdsKeyTag(statefulSetUID))<<32 | epoch, nil
}

// IsMDSKey reports whether key is in the range every metadata server key is,
// which includes the constant key nfsd registers without a key of its own.
func IsMDSKey(key uint64) bool {
	return key>>56 == mdsKeyTopByte
}

// ParseMDSKey splits a key MDSReservationKey made into its StatefulSet tag and
// its epoch. It reports false for a key outside the metadata server range, and
// and for any key whose epoch is 0, which includes nfsd's constant key.
func ParseMDSKey(key uint64) (tag, epoch uint32, ok bool) {
	epoch = uint32(key)
	if !IsMDSKey(key) || epoch == 0 {
		return 0, 0, false
	}
	return uint32(key >> 32 & 0xffffff), epoch, true
}

// MDSKeyTag is the 24-bit tag of a StatefulSet's keys, for a caller that
// compares a key it read against its own StatefulSet's.
func MDSKeyTag(statefulSetUID string) uint32 {
	return mdsKeyTag(statefulSetUID)
}

func mdsKeyTag(statefulSetUID string) uint32 {
	sum := sha256.Sum256([]byte(statefulSetUID))
	return uint32(sum[0])<<16 | uint32(sum[1])<<8 | uint32(sum[2])
}
