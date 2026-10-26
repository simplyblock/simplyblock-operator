// CreateBucket and DeleteBucket against versitygw's real posix backend over a
// temporary directory and a fake Kubernetes client. The tests play the
// operator's part: once an S3Bucket exists they create its directory, as
// DeliverBucket's mount would, and write its status.

package buckets

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/versity/versitygw/auth"
	"github.com/versity/versitygw/backend/meta"
	"github.com/versity/versitygw/backend/posix"
	"github.com/versity/versitygw/s3err"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	testNamespace   = "simplyblock"
	testObjectStore = "team-a-store"
)

// newTestBackend returns a Backend over a fresh posix root and a fake client
// seeded with objects, plus the client and the root.
func newTestBackend(t *testing.T, timeout time.Duration, objects ...client.Object) (*Backend, client.Client, string) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha2.S3Bucket{}).
		WithObjects(objects...).
		Build()

	root := t.TempDir()
	be, err := posix.New(root, meta.XattrMeta{}, posix.PosixOpts{BucketLinks: true, ValidateBucketNames: true})
	if err != nil {
		t.Fatalf("starting the posix backend: %v", err)
	}

	return New(be, cli, Settings{
		RootDir:          root,
		Namespace:        testNamespace,
		ObjectStore:      testObjectStore,
		StorageClassName: "simplyblock-pnfs",
		CapacityBytes:    10 << 30,
		Timeout:          timeout,
		PollInterval:     10 * time.Millisecond,
	}), cli, root
}

// provisionOnCreate plays the operator for one bucket: once its S3Bucket
// exists, it creates the bucket's directory under root when phase is Ready,
// and writes phase into the S3Bucket's status.
func provisionOnCreate(t *testing.T, cli client.Client, root, bucket string, phase simplyblockv1alpha2.S3BucketPhase) {
	t.Helper()
	go func() {
		key := client.ObjectKey{Namespace: testNamespace, Name: bucket}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var obj simplyblockv1alpha2.S3Bucket
			if err := cli.Get(context.Background(), key, &obj); err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if phase == simplyblockv1alpha2.S3BucketPhaseReady {
				if err := os.Mkdir(filepath.Join(root, bucket), 0o755); err != nil {
					t.Errorf("creating the bucket's directory: %v", err)
					return
				}
			}
			obj.Status.Phase = phase
			obj.Status.Message = "provisioned by the test"
			if err := cli.Status().Update(context.Background(), &obj); err != nil {
				t.Errorf("writing the S3Bucket's status: %v", err)
			}
			return
		}
		t.Errorf("S3Bucket %s was never created", bucket)
	}()
}

// existingBucket is an S3Bucket already in phase, owned by owner, for
// objectStore.
func existingBucket(name, objectStore, owner string, phase simplyblockv1alpha2.S3BucketPhase) *simplyblockv1alpha2.S3Bucket {
	return &simplyblockv1alpha2.S3Bucket{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   testNamespace,
			Annotations: map[string]string{simplyblockv1alpha2.S3BucketOwnerAnnotation: owner},
		},
		Spec: simplyblockv1alpha2.S3BucketSpec{
			ObjectStoreRef:   objectStore,
			BucketName:       name,
			CapacityBytes:    1 << 30,
			StorageClassName: "simplyblock-pnfs",
		},
		Status: simplyblockv1alpha2.S3BucketStatus{Phase: phase},
	}
}

func aclFor(t *testing.T, owner string) []byte {
	t.Helper()
	acl, err := json.Marshal(auth.ACL{Owner: owner})
	if err != nil {
		t.Fatalf("marshalling the ACL: %v", err)
	}
	return acl
}

func createInput(bucket string) *s3.CreateBucketInput {
	return &s3.CreateBucketInput{
		Bucket:                     aws.String(bucket),
		ObjectLockEnabledForBucket: aws.Bool(false),
		CreateBucketConfiguration:  &types.CreateBucketConfiguration{},
	}
}

// s3Code is the S3 error code versitygw would answer err with. versitygw
// type-asserts rather than unwrapping (s3api/controllers/base.go), so a
// wrapped S3 error would reach the client as InternalError.
func s3Code(t *testing.T, err error) (string, int) {
	t.Helper()
	serr, ok := err.(s3err.S3Error)
	if !ok {
		t.Fatalf("error %v (%T) is not an s3err.S3Error, versitygw would answer InternalError", err, err)
	}
	return serr.BaseError().Code, serr.StatusCode()
}

func bucketExists(t *testing.T, cli client.Client, name string) bool {
	t.Helper()
	err := cli.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &simplyblockv1alpha2.S3Bucket{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("reading the S3Bucket: %v", err)
	}
	return true
}

func TestCreateBucketProvisionsAnS3BucketAndAppliesTheRequest(t *testing.T) {
	b, cli, root := newTestBackend(t, 5*time.Second)
	ctx := context.Background()
	const bucket = "team-a-reports"
	provisionOnCreate(t, cli, root, bucket, simplyblockv1alpha2.S3BucketPhaseReady)

	input := createInput(bucket)
	input.ObjectOwnership = types.ObjectOwnershipBucketOwnerEnforced
	input.ObjectLockEnabledForBucket = aws.Bool(true)
	input.CreateBucketConfiguration.Tags = []types.Tag{{Key: aws.String("team"), Value: aws.String("a")}}
	acl := aclFor(t, "alice")

	if err := b.CreateBucket(ctx, input, acl); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	var obj simplyblockv1alpha2.S3Bucket
	if err := cli.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: bucket}, &obj); err != nil {
		t.Fatalf("reading the S3Bucket: %v", err)
	}
	want := simplyblockv1alpha2.S3BucketSpec{
		ObjectStoreRef: testObjectStore, BucketName: bucket, CapacityBytes: 10 << 30, StorageClassName: "simplyblock-pnfs",
	}
	if obj.Spec != want {
		t.Errorf("spec = %+v, want %+v", obj.Spec, want)
	}
	if got := obj.Annotations[simplyblockv1alpha2.S3BucketOwnerAnnotation]; got != "alice" {
		t.Errorf("owner annotation = %q, want alice", got)
	}

	gotACL, err := b.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: aws.String(bucket)})
	if err != nil || string(gotACL) != string(acl) {
		t.Errorf("GetBucketAcl = %q, %v, want %q", gotACL, err, acl)
	}
	ownership, err := b.GetBucketOwnershipControls(ctx, bucket)
	if err != nil || ownership != types.ObjectOwnershipBucketOwnerEnforced {
		t.Errorf("GetBucketOwnershipControls = %q, %v, want BucketOwnerEnforced", ownership, err)
	}
	tags, err := b.GetBucketTagging(ctx, bucket)
	if err != nil || tags["team"] != "a" {
		t.Errorf("GetBucketTagging = %v, %v, want team=a", tags, err)
	}
	lockJSON, err := b.GetObjectLockConfiguration(ctx, bucket)
	var lock auth.BucketLockConfig
	if err != nil || json.Unmarshal(lockJSON, &lock) != nil || !lock.Enabled {
		t.Errorf("GetObjectLockConfiguration = %s, %v, want object lock enabled", lockJSON, err)
	}
}

func TestCreateBucketDeletesTheS3BucketWhenItDegrades(t *testing.T) {
	b, cli, root := newTestBackend(t, 5*time.Second)
	const bucket = "degrades"
	provisionOnCreate(t, cli, root, bucket, simplyblockv1alpha2.S3BucketPhaseDegraded)

	err := b.CreateBucket(context.Background(), createInput(bucket), aclFor(t, "alice"))
	if code, status := s3Code(t, err); code != "InternalError" || status != 500 {
		t.Errorf("CreateBucket = %s/%d, want InternalError/500", code, status)
	}
	if bucketExists(t, cli, bucket) {
		t.Error("the Degraded S3Bucket is still there, want CreateBucket to delete it")
	}
}

func TestCreateBucketDeletesTheS3BucketWhenItTimesOut(t *testing.T) {
	b, cli, _ := newTestBackend(t, 100*time.Millisecond)
	const bucket = "never-ready"

	err := b.CreateBucket(context.Background(), createInput(bucket), aclFor(t, "alice"))
	if code, status := s3Code(t, err); code != "ServiceUnavailable" || status != 503 {
		t.Errorf("CreateBucket = %s/%d, want ServiceUnavailable/503", code, status)
	}
	if bucketExists(t, cli, bucket) {
		t.Error("the S3Bucket is still there, want CreateBucket to delete it after the timeout")
	}
}

func TestCreateBucketOnAnExistingS3Bucket(t *testing.T) {
	for _, tc := range []struct {
		name, objectStore, caller, want string
	}{
		{"same owner", testObjectStore, "alice", "BucketAlreadyOwnedByYou"},
		{"other owner", testObjectStore, "bob", "BucketAlreadyExists"},
		{"other ObjectStore", "team-b-store", "alice", "BucketAlreadyExists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := existingBucket("taken", tc.objectStore, "alice", simplyblockv1alpha2.S3BucketPhaseReady)
			b, _, _ := newTestBackend(t, time.Second, existing)

			err := b.CreateBucket(context.Background(), createInput("taken"), aclFor(t, tc.caller))
			if code, _ := s3Code(t, err); code != tc.want {
				t.Errorf("CreateBucket = %s, want %s", code, tc.want)
			}
		})
	}
}

func TestCreateBucketRejectsANameKubernetesCannotStore(t *testing.T) {
	b, cli, _ := newTestBackend(t, time.Second)

	err := b.CreateBucket(context.Background(), createInput("two..dots"), aclFor(t, "alice"))
	if code, _ := s3Code(t, err); code != "InvalidBucketName" {
		t.Errorf("CreateBucket = %s, want InvalidBucketName", code)
	}
	var list simplyblockv1alpha2.S3BucketList
	if err := cli.List(context.Background(), &list); err != nil || len(list.Items) != 0 {
		t.Errorf("S3Buckets = %d, %v, want none created", len(list.Items), err)
	}
}

func TestDeleteBucketRefusesANonEmptyBucket(t *testing.T) {
	b, cli, root := newTestBackend(t, time.Second,
		existingBucket("full", testObjectStore, "alice", simplyblockv1alpha2.S3BucketPhaseReady))
	if err := os.MkdirAll(filepath.Join(root, "full"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "full", "object"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := b.DeleteBucket(context.Background(), "full")
	if code, _ := s3Code(t, err); code != "BucketNotEmpty" {
		t.Errorf("DeleteBucket = %s, want BucketNotEmpty", code)
	}
	if !bucketExists(t, cli, "full") {
		t.Error("the S3Bucket of a non-empty bucket was deleted")
	}
}

func TestDeleteBucketWaitsUntilTheS3BucketIsGone(t *testing.T) {
	existing := existingBucket("empty", testObjectStore, "alice", simplyblockv1alpha2.S3BucketPhaseReady)
	existing.Finalizers = []string{simplyblockv1alpha2.S3BucketFinalizer}
	b, cli, root := newTestBackend(t, 5*time.Second, existing)
	// The posix backend's own scratch directory does not make a bucket
	// non-empty.
	if err := os.MkdirAll(filepath.Join(root, "empty", posix.MetaTmpDir), 0o755); err != nil {
		t.Fatal(err)
	}

	const cleanup = 200 * time.Millisecond
	go func() {
		time.Sleep(cleanup)
		var obj simplyblockv1alpha2.S3Bucket
		if err := cli.Get(context.Background(), client.ObjectKeyFromObject(existing), &obj); err != nil {
			t.Errorf("reading the S3Bucket to release it: %v", err)
			return
		}
		obj.Finalizers = nil
		if err := cli.Update(context.Background(), &obj); err != nil {
			t.Errorf("removing the finalizer: %v", err)
		}
	}()

	start := time.Now()
	if err := b.DeleteBucket(context.Background(), "empty"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if elapsed := time.Since(start); elapsed < cleanup {
		t.Errorf("DeleteBucket returned after %v, before the S3Bucket's finalizer was released", elapsed)
	}
	if bucketExists(t, cli, "empty") {
		t.Error("the S3Bucket is still there after DeleteBucket returned")
	}
}

func TestDeleteBucketTimesOutWhileTheS3BucketLingers(t *testing.T) {
	existing := existingBucket("stuck", testObjectStore, "alice", simplyblockv1alpha2.S3BucketPhaseReady)
	existing.Finalizers = []string{simplyblockv1alpha2.S3BucketFinalizer}
	b, _, root := newTestBackend(t, 100*time.Millisecond, existing)
	if err := os.Mkdir(filepath.Join(root, "stuck"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := b.DeleteBucket(context.Background(), "stuck")
	if code, status := s3Code(t, err); code != "ServiceUnavailable" || status != 503 {
		t.Errorf("DeleteBucket = %s/%d, want ServiceUnavailable/503", code, status)
	}
}

func TestDeleteBucketOfABucketThisGatewayDoesNotServe(t *testing.T) {
	b, cli, _ := newTestBackend(t, time.Second,
		existingBucket("elsewhere", "team-b-store", "alice", simplyblockv1alpha2.S3BucketPhaseReady))

	for _, bucket := range []string{"missing", "elsewhere"} {
		err := b.DeleteBucket(context.Background(), bucket)
		if code, _ := s3Code(t, err); code != "NoSuchBucket" {
			t.Errorf("DeleteBucket(%s) = %s, want NoSuchBucket", bucket, code)
		}
	}
	if !bucketExists(t, cli, "elsewhere") {
		t.Error("another ObjectStore's S3Bucket was deleted")
	}
}

func TestDeleteBucketWhileItIsStillProvisioning(t *testing.T) {
	b, cli, _ := newTestBackend(t, time.Second,
		existingBucket("in-flight", testObjectStore, "alice", simplyblockv1alpha2.S3BucketPhaseProvisioning))

	err := b.DeleteBucket(context.Background(), "in-flight")
	if code, status := s3Code(t, err); code != "OperationAborted" || status != 409 {
		t.Errorf("DeleteBucket = %s/%d, want OperationAborted/409", code, status)
	}
	if !bucketExists(t, cli, "in-flight") {
		t.Error("a provisioning S3Bucket was deleted under its CreateBucket")
	}
}
