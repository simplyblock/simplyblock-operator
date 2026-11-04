// S1: the fixed S3 lifecycle. The object half is tests/mc's sequence, the IAM
// half is the walkthrough in tests/manual. Both run in one order every time, so
// a failure names one operation.
//
// The S3 operations here are shared with the randomized spec in s3_random.go.

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	// s3PartSize is the multipart part size: S3's minimum for every part but
	// the last.
	s3PartSize = 5 << 20

	// iamCacheTTL is how long the gateway may answer from its cached copy of
	// an account after the admin API changed it (VGW_IAM_CACHE_TTL, 120s by
	// default), plus a margin.
	iamCacheTTL = 3 * time.Minute
)

func runS3Lifecycle(env *s3Env) {
	root := env.gw.s3Client(env.root)
	bucket := env.prefix + "-life"

	ginkgo.By("create bucket " + bucket + " and check its S3Bucket is Ready")
	framework.ExpectNoError(createBucket(root, bucket))
	framework.ExpectNoError(expectBucketReady(env, bucket))
	gomega.Expect(listBucketNames(root, env.prefix)).To(gomega.ContainElement(bucket))

	small := []byte("hello from the simplyblock S3 e2e suite\n")
	medium := randomBytes(5 << 20)
	large := randomBytes(20 << 20)

	ginkgo.By("put a small object, a 5 MiB object, and a 20 MiB multipart object")
	framework.ExpectNoError(putObject(root, bucket, "small.txt", small))
	framework.ExpectNoError(putObject(root, bucket, "medium.bin", medium))
	framework.ExpectNoError(putMultipart(root, bucket, "dir/large.bin", large))

	ginkgo.By("list the bucket")
	keys, err := listKeys(root, bucket, "")
	framework.ExpectNoError(err)
	gomega.Expect(keys).To(gomega.ConsistOf("small.txt", "medium.bin", "dir/large.bin"))

	ginkgo.By("stat and read back every object")
	for key, want := range map[string][]byte{"small.txt": small, "medium.bin": medium, "dir/large.bin": large} {
		framework.ExpectNoError(expectObjectSize(root, bucket, key, int64(len(want))))
		framework.ExpectNoError(expectObject(root, bucket, key, want))
	}

	ginkgo.By("overwrite the small object")
	replaced := []byte("replaced\n")
	framework.ExpectNoError(putObject(root, bucket, "small.txt", replaced))
	framework.ExpectNoError(expectObject(root, bucket, "small.txt", replaced))

	ginkgo.By("copy the medium object server-side")
	framework.ExpectNoError(copyObject(root, bucket, "medium.bin", bucket, "medium-copy.bin"))
	framework.ExpectNoError(expectObject(root, bucket, "medium-copy.bin", medium))

	ginkgo.By("a bucket with objects in it refuses deletion")
	framework.ExpectNoError(expectS3Error(deleteBucket(root, bucket), "BucketNotEmpty"))

	ginkgo.By("delete every object in one request")
	framework.ExpectNoError(deleteObjects(root, bucket,
		[]string{"small.txt", "medium.bin", "medium-copy.bin", "dir/large.bin"}))
	keys, err = listKeys(root, bucket, "")
	framework.ExpectNoError(err)
	gomega.Expect(keys).To(gomega.BeEmpty())

	ginkgo.By("delete the bucket and check its S3Bucket is gone")
	framework.ExpectNoError(deleteBucket(root, bucket))
	framework.ExpectNoError(waitForBucketGone(env, bucket))
	gomega.Expect(listBucketNames(root, env.prefix)).NotTo(gomega.ContainElement(bucket))
}

func runS3IAMLifecycle(env *s3Env) {
	gw := env.gw
	alice := s3Account{access: env.prefix + "-alice", secret: randomHex(20)}
	bob := s3Account{access: env.prefix + "-bob", secret: randomHex(20)}

	ginkgo.By("create " + alice.access + " as userplus and " + bob.access + " as user")
	framework.ExpectNoError(gw.createUser(env.root, alice, roleUserPlus))
	env.users[alice.access] = true
	framework.ExpectNoError(gw.createUser(env.root, bob, roleUser))
	env.users[bob.access] = true
	asAlice, asBob, asRoot := gw.s3Client(alice), gw.s3Client(bob), gw.s3Client(env.root)

	aliceBucket := env.prefix + "-alice"
	ginkgo.By("userplus creates a bucket, user may not")
	framework.ExpectNoError(createBucket(asAlice, aliceBucket))
	framework.ExpectNoError(expectBucketReady(env, aliceBucket))
	framework.ExpectNoError(expectS3Error(createBucket(asBob, env.prefix+"-bob"), "AccessDenied"))

	ginkgo.By("alice's data is hers alone")
	data := []byte("alice's\n")
	framework.ExpectNoError(putObject(asAlice, aliceBucket, "note.txt", data))
	framework.ExpectNoError(expectObject(asAlice, aliceBucket, "note.txt", data))
	_, err := listKeys(asBob, aliceBucket, "")
	framework.ExpectNoError(expectS3Error(err, "AccessDenied"))
	framework.ExpectNoError(expectS3Error(getObjectErr(asBob, aliceBucket, "note.txt"), "AccessDenied"))
	gomega.Expect(listBucketNames(asBob, env.prefix)).To(gomega.BeEmpty())
	gomega.Expect(listBucketNames(asRoot, env.prefix)).To(gomega.ContainElement(aliceBucket))

	owners, err := gw.bucketOwners(env.root)
	framework.ExpectNoError(err)
	gomega.Expect(owners).To(gomega.HaveKeyWithValue(aliceBucket, alice.access))

	bobBucket := env.prefix + "-bob"
	ginkgo.By("promote bob to userplus, after which he can create a bucket")
	framework.ExpectNoError(gw.updateUserRole(env.root, bob.access, roleUserPlus))
	framework.ExpectNoError(eventually(iamCacheTTL, func() error {
		return createBucket(asBob, bobBucket)
	}), "bob still cannot create a bucket %s after his promotion", iamCacheTTL)
	framework.ExpectNoError(expectBucketReady(env, bobBucket))

	ginkgo.By("hand bob's bucket to alice")
	framework.ExpectNoError(gw.changeBucketOwner(env.root, bobBucket, alice.access))
	framework.ExpectNoError(eventually(iamCacheTTL, func() error {
		names, err := listBucketNames(asAlice, env.prefix)
		if err == nil && !slices.Contains(names, bobBucket) {
			err = fmt.Errorf("alice lists %v, without %s", names, bobBucket)
		}
		return err
	}))
	framework.ExpectNoError(putObject(asAlice, bobBucket, "moved.txt", data))

	ginkgo.By("delete bob, after which his key is refused")
	framework.ExpectNoError(gw.deleteUser(env.root, bob.access))
	delete(env.users, bob.access)
	framework.ExpectNoError(eventually(iamCacheTTL, func() error {
		_, err := asBob.ListBuckets(context.Background(), &s3.ListBucketsInput{})
		if code := s3ErrorCode(err); code == "InvalidAccessKeyId" || code == "AccessDenied" {
			return nil
		}
		return fmt.Errorf("bob's ListBuckets answered %v after his deletion", err)
	}))

	ginkgo.By("alice empties and deletes both buckets")
	for _, bucket := range []string{aliceBucket, bobBucket} {
		framework.ExpectNoError(emptyAndDeleteBucket(asAlice, bucket))
		framework.ExpectNoError(waitForBucketGone(env, bucket))
	}
	framework.ExpectNoError(gw.deleteUser(env.root, alice.access))
	delete(env.users, alice.access)
}

// eventually retries fn until it succeeds or timeout passes, and returns its
// last error.
func eventually(timeout time.Duration, fn func() error) error {
	var last error
	_ = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, timeout, true,
		func(context.Context) (bool, error) {
			last = fn()
			return last == nil, nil
		})
	return last
}

func opContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s3OpTimeout)
}

func createBucket(c *s3.Client, bucket string) error {
	ctx, cancel := opContext()
	defer cancel()
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	return err
}

func deleteBucket(c *s3.Client, bucket string) error {
	ctx, cancel := opContext()
	defer cancel()
	_, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	return err
}

// listBucketNames is the caller's buckets that start with prefix. Buckets of
// other ObjectStores share the mount and can be listed too.
func listBucketNames(c *s3.Client, prefix string) ([]string, error) {
	ctx, cancel := opContext()
	defer cancel()
	out, err := c.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, fmt.Errorf("ListBuckets: %w", err)
	}
	names := []string{}
	for _, b := range out.Buckets {
		if strings.HasPrefix(aws.ToString(b.Name), prefix) {
			names = append(names, aws.ToString(b.Name))
		}
	}
	return names, nil
}

func putObject(c *s3.Client, bucket, key string, data []byte) error {
	ctx, cancel := opContext()
	defer cancel()
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("PutObject %s/%s (%d bytes): %w", bucket, key, len(data), err)
	}
	return nil
}

// putMultipart uploads data in s3PartSize parts, so the gateway assembles it
// on CompleteMultipartUpload (issues/01 is that step hanging).
func putMultipart(c *s3.Client, bucket, key string, data []byte) error {
	ctx, cancel := opContext()
	defer cancel()
	created, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("CreateMultipartUpload %s/%s: %w", bucket, key, err)
	}
	var parts []s3types.CompletedPart
	for offset, number := 0, int32(1); offset < len(data); offset, number = offset+s3PartSize, number+1 {
		end := min(offset+s3PartSize, len(data))
		part, err := c.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: created.UploadId,
			PartNumber: aws.Int32(number), Body: bytes.NewReader(data[offset:end]),
		})
		if err != nil {
			return fmt.Errorf("UploadPart %d of %s/%s: %w", number, bucket, key, err)
		}
		parts = append(parts, s3types.CompletedPart{ETag: part.ETag, PartNumber: aws.Int32(number)})
	}
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: created.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return fmt.Errorf("CompleteMultipartUpload %s/%s (%d parts): %w", bucket, key, len(parts), err)
	}
	return nil
}

func getObject(c *s3.Client, bucket, key string, byteRange *string) ([]byte, error) {
	ctx, cancel := opContext()
	defer cancel()
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Range: byteRange})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	return io.ReadAll(out.Body)
}

func getObjectErr(c *s3.Client, bucket, key string) error {
	_, err := getObject(c, bucket, key, nil)
	return err
}

// expectObject reads an object back and compares it byte for byte.
func expectObject(c *s3.Client, bucket, key string, want []byte) error {
	got, err := getObject(c, bucket, key, nil)
	if err != nil {
		return fmt.Errorf("GetObject %s/%s: %w", bucket, key, err)
	}
	return compareBytes(bucket+"/"+key, got, want)
}

func compareBytes(what string, got, want []byte) error {
	if bytes.Equal(got, want) {
		return nil
	}
	at := 0
	for at < len(got) && at < len(want) && got[at] == want[at] {
		at++
	}
	return fmt.Errorf("%s read back %d bytes, want %d; first difference at offset %d", what, len(got), len(want), at)
}

func expectObjectSize(c *s3.Client, bucket, key string, size int64) error {
	ctx, cancel := opContext()
	defer cancel()
	out, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("HeadObject %s/%s: %w", bucket, key, err)
	}
	if got := aws.ToInt64(out.ContentLength); got != size {
		return fmt.Errorf("HeadObject %s/%s reports %d bytes, want %d", bucket, key, got, size)
	}
	return nil
}

func copyObject(c *s3.Client, srcBucket, srcKey, dstBucket, dstKey string) error {
	ctx, cancel := opContext()
	defer cancel()
	_, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(dstBucket), Key: aws.String(dstKey),
		CopySource: aws.String(copySource(srcBucket, srcKey)),
	})
	if err != nil {
		return fmt.Errorf("CopyObject %s/%s to %s/%s: %w", srcBucket, srcKey, dstBucket, dstKey, err)
	}
	return nil
}

// copySource is CopyObject's x-amz-copy-source value for an object. S3 reads
// the header URL-encoded and the SDK sends it as given, so the key is escaped
// here. Everything but unreserved characters and the separator is escaped,
// because url.PathEscape keeps the plus sign, which a server decodes as a space.
func copySource(bucket, key string) string {
	var b strings.Builder
	b.WriteString(bucket)
	b.WriteByte('/')
	for _, c := range []byte(key) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func deleteObject(c *s3.Client, bucket, key string) error {
	ctx, cancel := opContext()
	defer cancel()
	_, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("DeleteObject %s/%s: %w", bucket, key, err)
	}
	return nil
}

// deleteObjects removes keys in one DeleteObjects request and fails on any
// key the answer reports as not deleted.
func deleteObjects(c *s3.Client, bucket string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	ctx, cancel := opContext()
	defer cancel()
	ids := make([]s3types.ObjectIdentifier, 0, len(keys))
	for _, key := range keys {
		ids = append(ids, s3types.ObjectIdentifier{Key: aws.String(key)})
	}
	out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(bucket), Delete: &s3types.Delete{Objects: ids},
	})
	if err != nil {
		return fmt.Errorf("DeleteObjects %s (%d keys): %w", bucket, len(keys), err)
	}
	if len(out.Errors) > 0 {
		e := out.Errors[0]
		return fmt.Errorf("DeleteObjects %s: %d keys not deleted, first %s: %s %s",
			bucket, len(out.Errors), aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
	}
	return nil
}

// listKeys is every key under prefix, read in pages of at most 7 keys so the
// continuation path is exercised too.
func listKeys(c *s3.Client, bucket, prefix string) ([]string, error) {
	ctx, cancel := opContext()
	defer cancel()
	keys := []string{}
	pages := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(7),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, object := range page.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
	}
	return keys, nil
}

// emptyAndDeleteBucket aborts the bucket's open multipart uploads, deletes
// every object, and deletes the bucket.
func emptyAndDeleteBucket(c *s3.Client, bucket string) error {
	ctx, cancel := opContext()
	defer cancel()
	uploads, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
	if err == nil {
		for _, u := range uploads.Uploads {
			_, _ = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket: aws.String(bucket), Key: u.Key, UploadId: u.UploadId,
			})
		}
	}
	for {
		keys, err := listKeys(c, bucket, "")
		if err != nil {
			return fmt.Errorf("list %s: %w", bucket, err)
		}
		if len(keys) == 0 {
			break
		}
		for start := 0; start < len(keys); start += 1000 {
			if err := deleteObjects(c, bucket, keys[start:min(start+1000, len(keys))]); err != nil {
				return err
			}
		}
	}
	return deleteBucket(c, bucket)
}
