// Deterministic object names derived from a bucket name (design-bucket-
// provisioning.md §4.1): the backing PVC is named from spec.bucketName
// rather than from the S3Bucket's own metadata.name, because the two can
// differ and a PVC must be findable from the bucket name alone.
//
// The S3 bucket grammar (`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`) is looser than
// a Kubernetes object name in a way atlas-lib's kube.Formula does not close
// on its own: a run of two or more dots or hyphens is S3-legal but makes a
// DNS-1123 subdomain carry an empty or malformed label, and Formula's own
// sanitizer only strips characters outside the charset, not that structure.
// pvcNameForCollapsesSeparatorRuns closes the remaining gap.

package objectstore

import (
	"regexp"

	atlaskube "github.com/simplyblock/atlas/kube"
)

// s3BucketPVCNames derives a bucket's backing PVC name for the ordinary
// case: no digest, matching design §4.1's example
// (team-a-reports -> s3bucket-team-a-reports) exactly, for any name that
// carries no separator run for collapseSeparatorRuns to ever touch.
var s3BucketPVCNames = atlaskube.Formula{Prefix: "s3bucket-"}

// s3BucketPVCNamesDigested is the same formula with AlwaysDigest set, used
// only when collapsing a separator run could make two distinct bucket names
// produce the same stem (U-18): the digest is taken over the bucket name
// before collapsing, so it still tells them apart after.
var s3BucketPVCNamesDigested = atlaskube.Formula{Prefix: "s3bucket-", AlwaysDigest: true}

// separatorRun matches two or more consecutive dots or hyphens: a single one
// of either, between the alphanumerics the S3 grammar requires at both ends
// of a bucket name, is already a legal DNS-1123 subdomain boundary, but a run
// of two or more can carry an empty or malformed label a single one cannot.
var separatorRun = regexp.MustCompile(`[.-]{2,}`)

// pvcNameFor derives the backing PersistentVolumeClaim's name from a bucket
// name.
func pvcNameFor(bucketName string) string {
	if !separatorRun.MatchString(bucketName) {
		return s3BucketPVCNames.Derive(bucketName).Value
	}
	// The digest must be taken over the original name, before collapsing: two
	// names whose only difference is which separators form the run (e.g.
	// "a..b" and "a.-b") would otherwise collapse to the one stem "a-b" and
	// collide.
	digested := s3BucketPVCNamesDigested.Derive(bucketName).Value
	return separatorRun.ReplaceAllString(digested, "-")
}
