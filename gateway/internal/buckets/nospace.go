// Reclassifies the ExistingObjectIsDirectory error versitygw's posix backend
// returns for any failed write, so a full bucket volume answers 507
// InsufficientStorage. It lives beside Backend because the gateway's own
// wrapper of the posix backend is the only place the error can be corrected
// without patching versitygw.
//
// versitygw v1.9.0 maps every error from linking a finished object into place
// to ExistingObjectIsDirectory (backend/posix/posix.go, PutObjectWithPostFunc
// and CompleteMultipartUploadWithCopy). On a pNFS mount a full volume surfaces
// exactly there: write() only fills the client's page cache, and the server's
// ENOSPC comes back from the fsync and close inside the link. The errno is
// discarded, so the cause is re-derived from the filesystem instead.

package buckets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/versity/versitygw/s3err"
	"github.com/versity/versitygw/s3response"
	"golang.org/x/sys/unix"
)

// lowSpaceDivisor sets the free-space floor for a write whose size is not
// known up front: a volume with less than 1/lowSpaceDivisor of its capacity
// left counts as full.
const lowSpaceDivisor = 100

// PutObject is the posix backend's PutObject with a full volume reported as
// InsufficientStorage.
func (b *Backend) PutObject(ctx context.Context, input s3response.PutObjectInput) (s3response.PutObjectOutput, error) {
	out, err := b.Backend.PutObject(ctx, input)
	return out, b.classifyWriteErr(err, aws.ToString(input.Bucket), aws.ToString(input.Key), aws.ToInt64(input.ContentLength))
}

// CopyObject is the posix backend's CopyObject with a full volume reported as
// InsufficientStorage. The posix backend's CopyObject calls its own PutObject,
// not this Backend's, so it needs the same correction.
func (b *Backend) CopyObject(ctx context.Context, input s3response.CopyObjectInput) (s3response.CopyObjectOutput, error) {
	out, err := b.Backend.CopyObject(ctx, input)
	return out, b.classifyWriteErr(err, aws.ToString(input.Bucket), aws.ToString(input.Key), 0)
}

// CompleteMultipartUpload is the posix backend's CompleteMultipartUpload with
// a full volume reported as InsufficientStorage.
func (b *Backend) CompleteMultipartUpload(
	ctx context.Context, input *s3.CompleteMultipartUploadInput,
) (s3response.CompleteMultipartUploadResult, string, error) {
	out, version, err := b.Backend.CompleteMultipartUpload(ctx, input)
	return out, version, b.classifyWriteErr(err, aws.ToString(input.Bucket), aws.ToString(input.Key), 0)
}

// classifyWriteErr returns err unchanged unless it is ExistingObjectIsDirectory
// and the key is not blocked by the filesystem layout. Then it is
// InsufficientStorage when the bucket's volume has less than size bytes, or
// less than 1/lowSpaceDivisor of its capacity, available, and InternalError
// otherwise. The free space is read after the failed write's temporary file
// is removed, so a write that did not fit still finds less than its own size
// free.
func (b *Backend) classifyWriteErr(err error, bucket, key string, size int64) error {
	var apiErr s3err.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != s3err.GetAPIError(s3err.ErrExistingObjectIsDirectory).Code {
		return err
	}
	bucketDir := filepath.Join(b.settings.RootDir, bucket)
	if keyBlocked(bucketDir, key) {
		return err
	}
	avail, total, serr := b.statfs(bucketDir)
	if serr != nil {
		return internalError("storing the object failed, and the bucket's free space could not be read: " + serr.Error())
	}
	floor := total / lowSpaceDivisor
	if size > 0 && uint64(size) > floor {
		floor = uint64(size)
	}
	if avail < floor {
		return s3err.GetAPIError(s3err.ErrNoSpaceLeftOnDevice)
	}
	return internalError("storing the object failed for a reason the storage backend did not report")
}

// keyBlocked reports whether the layout under bucketDir really prevents key
// from being stored as a file: key itself is a directory, or one of its
// parents exists as something other than a directory.
func keyBlocked(bucketDir, key string) bool {
	path := bucketDir
	parts := strings.Split(strings.Trim(key, "/"), "/")
	for i, part := range parts {
		path = filepath.Join(path, part)
		fi, err := os.Lstat(path)
		if err != nil {
			return false
		}
		last := i == len(parts)-1
		if last && fi.IsDir() || !last && !fi.IsDir() {
			return true
		}
	}
	return false
}

// statfs is the Backend.statfs default: the filesystem holding path, as the
// unprivileged caller sees it.
func statfs(path string) (avail, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}
