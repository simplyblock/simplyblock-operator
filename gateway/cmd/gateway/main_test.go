// Proves run (main.go) actually serves S3 over the wire: a real aws-sdk-go-v2
// client, talking HTTP to a gateway run has started against a temporary
// directory, round-trips an object byte-for-byte, and separately proves
// --bucketlinks (docs/adr/0002-bucket-per-volume-via-bucketlinks.md) — the
// shape this project's buckets actually take, a volume symlinked in rather
// than created through CreateBucket — by symlinking a directory in and
// listing it back as a bucket nothing here ever created through the API.

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/simplyblock/gateway/internal/config"
)

const (
	testAccessKeyID     = "test-access-key"
	testSecretAccessKey = "test-secret-key"
)

// startGateway runs the gateway against a fresh temp directory on a free
// loopback port, waits for its health endpoint to answer, and returns an S3
// client pointed at it. The gateway is stopped, and run's own shutdown
// awaited, when the test ends.
func startGateway(t *testing.T) (*s3.Client, string) {
	t.Helper()

	addr := freeLoopbackAddr(t)
	cfg := config.Config{
		RootDir:             t.TempDir(),
		Port:                addr,
		RootAccessKeyID:     testAccessKeyID,
		RootSecretAccessKey: testSecretAccessKey,
		BucketLinks:         true,
		Fsync:               true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run did not shut down cleanly: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("run did not shut down within 10s of cancellation")
		}
	})

	waitForHealth(t, "http://"+addr+healthPath)

	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String("http://" + addr),
		Credentials:  awscreds.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, ""),
		UsePathStyle: true,
	})
	return client, cfg.RootDir
}

// freeLoopbackAddr finds a currently unused loopback port by binding to :0
// and releasing it, the standard (slightly racy, in exchange for not needing
// RunVersityGW to report back whatever port it actually bound) way to hand a
// subprocess-style component a concrete address to listen on next.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}
	return addr
}

func waitForHealth(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx // bounded by the deadline loop itself
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("health check: status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("gateway never became healthy at %s: %v", url, lastErr)
}

// TestRunServesAPutGetRoundTrip proves the composition in run (posix backend
// + embedgw) actually serves S3 end to end: a bucket created through the API,
// an object PUT, and the same bytes GET back.
func TestRunServesAPutGetRoundTrip(t *testing.T) {
	client, _ := startGateway(t)
	ctx := context.Background()

	const bucket = "roundtrip-bucket"
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	want := []byte("hello from the simplyblock gateway")
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("greeting.txt"),
		Body:   bytes.NewReader(want),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("greeting.txt"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer got.Body.Close()
	gotBytes, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("reading the object body: %v", err)
	}
	if !bytes.Equal(gotBytes, want) {
		t.Errorf("round-tripped object = %q, want %q", gotBytes, want)
	}
}

// TestRunRecognizesABucketlinksSymlink proves --bucketlinks itself (posix.
// PosixOpts.BucketLinks, true here): a directory this test symlinks under
// RootDir, exactly the shape an S3Bucket's own volume takes
// (design-bucket-provisioning.md §5), is listed back as a bucket, though
// nothing ever called CreateBucket for it.
func TestRunRecognizesABucketlinksSymlink(t *testing.T) {
	client, rootDir := startGateway(t)
	ctx := context.Background()

	volumeDir := filepath.Join(t.TempDir(), "team-a-reports-volume")
	if err := os.Mkdir(volumeDir, 0o755); err != nil {
		t.Fatalf("creating the volume directory: %v", err)
	}
	const bucketName = "team-a-reports"
	if err := os.Symlink(volumeDir, filepath.Join(rootDir, bucketName)); err != nil {
		t.Fatalf("symlinking the bucket in: %v", err)
	}

	out, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	var found bool
	for _, b := range out.Buckets {
		if aws.ToString(b.Name) == bucketName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListBuckets did not include %q, the symlinked bucket; BucketLinks is not taking effect", bucketName)
	}
}
