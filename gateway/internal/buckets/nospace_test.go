// The writes whose ExistingObjectIsDirectory error Backend reclassifies, over
// a fake inner backend that fails every write the way versitygw's posix
// backend does when the bucket's volume is full, and a stubbed statfs.

package buckets

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/versity/versitygw/backend"
	"github.com/versity/versitygw/s3err"
	"github.com/versity/versitygw/s3response"
)

// fullVolume fails every write with ExistingObjectIsDirectory, which is what
// versitygw v1.9.0's posix backend returns for any error from linking the
// finished object into place, ENOSPC included.
type fullVolume struct {
	backend.BackendUnsupported
}

func (fullVolume) PutObject(context.Context, s3response.PutObjectInput) (s3response.PutObjectOutput, error) {
	return s3response.PutObjectOutput{}, s3err.GetAPIError(s3err.ErrExistingObjectIsDirectory)
}

func (fullVolume) CopyObject(context.Context, s3response.CopyObjectInput) (s3response.CopyObjectOutput, error) {
	return s3response.CopyObjectOutput{}, s3err.GetAPIError(s3err.ErrExistingObjectIsDirectory)
}

func (fullVolume) CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput) (s3response.CompleteMultipartUploadResult, string, error) {
	return s3response.CompleteMultipartUploadResult{}, "", s3err.GetAPIError(s3err.ErrExistingObjectIsDirectory)
}

// newFullVolumeBackend is a Backend over fullVolume whose bucket volume
// reports avail of total bytes free.
func newFullVolumeBackend(t *testing.T, avail, total uint64) (*Backend, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "reports"), 0o755); err != nil {
		t.Fatalf("creating the bucket directory: %v", err)
	}
	b := New(fullVolume{}, nil, Settings{RootDir: root})
	b.statfs = func(string) (uint64, uint64, error) { return avail, total, nil }
	return b, root
}

// The three writes, each called against bucket "reports" and key.
var writes = map[string]func(*Backend, string) error{
	"PutObject": func(b *Backend, key string) error {
		_, err := b.PutObject(context.Background(), s3response.PutObjectInput{
			Bucket: aws.String("reports"), Key: aws.String(key), ContentLength: aws.Int64(64 << 20),
		})
		return err
	},
	"CopyObject": func(b *Backend, key string) error {
		_, err := b.CopyObject(context.Background(), s3response.CopyObjectInput{
			Bucket: aws.String("reports"), Key: aws.String(key), CopySource: aws.String("reports/source"),
		})
		return err
	},
	"CompleteMultipartUpload": func(b *Backend, key string) error {
		_, _, err := b.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
			Bucket: aws.String("reports"), Key: aws.String(key), UploadId: aws.String("upload"),
		})
		return err
	},
}

// Regression: 2026-11-23-s3-full-bucket-reports-directory. A write into a
// full bucket volume answered 409 ExistingObjectIsDirectory, which no S3
// client treats as out of space, so clients neither stopped nor alerted (CI
// run 45100042373, TC-S3-003). A full volume must answer 507
// InsufficientStorage.
func TestWriteToAFullVolumeIsInsufficientStorage(t *testing.T) {
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			b, _ := newFullVolumeBackend(t, 1<<20, 1<<30)
			code, status := s3Code(t, write(b, "fill/00014"))
			if code != "InsufficientStorage" || status != 507 {
				t.Errorf("full volume answered %d %s, want 507 InsufficientStorage", status, code)
			}
		})
	}
}

// A key that really is a directory keeps versitygw's own 409, whatever the
// free space.
func TestWriteOverADirectoryStaysExistingObjectIsDirectory(t *testing.T) {
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			b, root := newFullVolumeBackend(t, 1<<20, 1<<30)
			if err := os.MkdirAll(filepath.Join(root, "reports", "fill", "00014"), 0o755); err != nil {
				t.Fatalf("creating the directory: %v", err)
			}
			code, status := s3Code(t, write(b, "fill/00014"))
			if code != "ExistingObjectIsDirectory" || status != 409 {
				t.Errorf("write over a directory answered %d %s, want 409 ExistingObjectIsDirectory", status, code)
			}
		})
	}
}

// A key whose parent is an object keeps versitygw's own 409 too: the key
// cannot be created as a file below another file.
func TestWriteBelowAnObjectStaysExistingObjectIsDirectory(t *testing.T) {
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			b, root := newFullVolumeBackend(t, 1<<20, 1<<30)
			if err := os.WriteFile(filepath.Join(root, "reports", "fill"), nil, 0o644); err != nil {
				t.Fatalf("creating the object: %v", err)
			}
			code, status := s3Code(t, write(b, "fill/00014"))
			if code != "ExistingObjectIsDirectory" || status != 409 {
				t.Errorf("write below an object answered %d %s, want 409 ExistingObjectIsDirectory", status, code)
			}
		})
	}
}

// With neither a directory in the way nor the volume full, the cause is
// unknown, which is an InternalError rather than a claim about the key.
func TestWriteFailureWithSpaceLeftIsInternalError(t *testing.T) {
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			b, _ := newFullVolumeBackend(t, 900<<20, 1<<30)
			code, status := s3Code(t, write(b, "fill/00014"))
			if code != "InternalError" || status != 500 {
				t.Errorf("unexplained write failure answered %d %s, want 500 InternalError", status, code)
			}
		})
	}
}
