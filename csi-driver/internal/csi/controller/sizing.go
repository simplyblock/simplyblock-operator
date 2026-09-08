// Capacity rounding. The control plane provisions in whole GiB, and a volume
// reported smaller than it was asked for fails the external provisioner's own
// check, so the create and the expand path round the same way.

package controller

const (
	mib = int64(1024 * 1024)
	gib = mib * 1024
)

// toGiB rounds up bytes to gigabytes
func toGiB(bytes int64) int64 {
	return (bytes + gib - 1) / gib
}

// alignToGiBBytes rounds bytes up to the next GiB boundary and returns bytes.
func alignToGiBBytes(bytes int64) int64 {
	return toGiB(bytes) * gib
}
