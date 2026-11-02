// Checks that posixOpts (main.go) hands the posix backend the bucket-shape
// settings config.FromEnv read, and the two this gateway fixes itself: bucket
// name validation, and the copy_file_range opt-out that keeps
// CompleteMultipartUpload off a path that hangs on the pNFS volumes every
// bucket is backed by.

package main

import (
	"testing"

	"github.com/simplyblock/gateway/internal/config"
)

// Regression: 2026-11-02-gateway-multipart-copy-file-range — CompleteMultipart
// Upload never returned on a pNFS-backed bucket. The posix backend reassembled
// the parts with io.Copy between two *os.File, which Linux turns into
// copy_file_range and an NFSv4.2 mount turns into a server-side async COPY
// whose completion is never reported, so the request hung in the kernel and
// the gateway leaked a worker per multipart upload until the pod was killed.
//
// The hang itself needs a real NFSv4.2 mount and is not reachable from a Go
// test, so this pins the nearest observable contract: what the gateway
// configures its backend with. The live-cluster reproduction is E-04 in
// operator/docs/tests/test-plan-objectstore.md.
func TestPosixOptsCarriesTheBucketShapeSettings(t *testing.T) {
	for _, tc := range []struct {
		name        string
		bucketLinks bool
		fsync       bool
	}{
		{"both enabled", true, true},
		{"both disabled", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := posixOpts(config.Config{BucketLinks: tc.bucketLinks, Fsync: tc.fsync})

			if got.BucketLinks != tc.bucketLinks {
				t.Errorf("BucketLinks = %t, want %t", got.BucketLinks, tc.bucketLinks)
			}
			if got.Fsync != tc.fsync {
				t.Errorf("Fsync = %t, want %t", got.Fsync, tc.fsync)
			}
			if !got.ValidateBucketNames {
				t.Error("ValidateBucketNames = false; a symlinked-in bucket's name would never be checked")
			}
			if !got.ForceNoCopyFileRange {
				t.Error("ForceNoCopyFileRange = false; CompleteMultipartUpload would reassemble parts " +
					"with copy_file_range, which hangs on an NFSv4.2 mount")
			}
		})
	}
}
