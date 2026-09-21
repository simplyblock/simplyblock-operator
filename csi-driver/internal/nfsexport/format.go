// The on-disk format an export's filesystem is made with.
//
// This image is newer than the hosts it runs on, so mkfs.xfs here defaults
// features an older host kernel cannot mount: the filesystem formats cleanly
// and then fails to mount, naming a feature flag rather than the skew behind
// it. Pinning the set fixes that, and unlike borrowing the host's binary it can
// be tested.

package nfsexport

// xfsFormatOptions pin the format to what every supported host kernel mounts.
// Each name records the kernel that gained it, because the rule for changing
// this list is the oldest kernel supported, not the newest one run on:
//
//   - crc is metadata checksumming, universal since 3.15, left on.
//   - bigtime and inobtcount arrived in 5.10.
//   - nrext64 arrived in 5.19 and is defaulted ON by the xfsprogs 6.x this
//     image ships. It is the one that bit: a host on 5.14 answers "Superblock
//     has unknown incompatible features."
//   - reflink is not needed by an export, and off keeps the format narrow.
var xfsFormatOptions = []string{
	"-m", "crc=1,bigtime=0,inobtcount=0,reflink=0",
	"-i", "nrext64=0",
}
