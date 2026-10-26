// Backend makes S3 CreateBucket and DeleteBucket drive an S3Bucket's
// lifecycle instead of a directory's: each bucket is its own volume
// (docs/adr/0002-bucket-per-volume-via-bucketlinks.md), which only the
// operator can provision, so CreateBucket creates the S3Bucket and waits for
// its volume to be mounted under the gateway's root, and DeleteBucket deletes
// the S3Bucket and waits for it to be gone. Every other call goes to the
// posix backend unchanged.

package buckets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/versity/versitygw/auth"
	"github.com/versity/versitygw/backend"
	"github.com/versity/versitygw/backend/posix"
	"github.com/versity/versitygw/s3err"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// rollbackTimeout bounds the S3Bucket delete a failed CreateBucket issues.
// It runs on its own context: the request's may be the reason it failed.
const rollbackTimeout = 30 * time.Second

// errServiceUnavailable answers a CreateBucket or DeleteBucket whose
// S3Bucket did not reach the wanted state within Settings.Timeout. s3err
// has no such code of its own.
var errServiceUnavailable = s3err.APIError{
	Code:           "ServiceUnavailable",
	Description:    "The bucket's storage was not ready in time. Please retry.",
	HTTPStatusCode: http.StatusServiceUnavailable,
}

// errOperationAborted answers a DeleteBucket for a bucket whose
// CreateBucket is still in progress, the code S3 uses for that conflict.
var errOperationAborted = s3err.APIError{
	Code:           "OperationAborted",
	Description:    "A conflicting conditional operation is currently in progress against this resource. Please try again.",
	HTTPStatusCode: http.StatusConflict,
}

// Settings is what Backend fills each S3Bucket from and how long it waits.
type Settings struct {
	// RootDir is the posix backend's root, where each bucket's volume is
	// mounted.
	RootDir string

	// Namespace and ObjectStore are where S3Buckets are created and which
	// ObjectStore they name.
	Namespace   string
	ObjectStore string

	// StorageClassName and CapacityBytes become each S3Bucket's spec.
	StorageClassName string
	CapacityBytes    int64

	// Timeout bounds each wait, and PollInterval is how often it re-reads
	// the S3Bucket.
	Timeout      time.Duration
	PollInterval time.Duration
}

// Backend is the posix backend with CreateBucket and DeleteBucket replaced.
type Backend struct {
	backend.Backend
	client   client.Client
	settings Settings
}

// New wraps be. The client needs get, create, and delete on S3Buckets in
// settings.Namespace, which the operator grants the gateway's
// ServiceAccount.
func New(be backend.Backend, cli client.Client, settings Settings) *Backend {
	return &Backend{Backend: be, client: cli, settings: settings}
}

// CreateBucket creates the bucket's S3Bucket, waits until the operator
// reports it Ready, then stores the request's ACL, ownership, tags, and
// object-lock setting on the mounted volume, as posix.CreateBucket would on
// a directory. If the S3Bucket degrades, does not become Ready within
// Settings.Timeout, or the metadata cannot be stored, the S3Bucket is
// deleted again.
func (b *Backend) CreateBucket(ctx context.Context, input *s3.CreateBucketInput, acl []byte) error {
	bucket := aws.ToString(input.Bucket)
	// The bucket name is the S3Bucket's name, so it must also be a valid
	// Kubernetes object name. The few S3-legal names that are not, such as
	// one with a period next to a hyphen, are refused.
	if len(validation.IsDNS1123Subdomain(bucket)) > 0 {
		return s3err.GetBucketErr(s3err.ErrInvalidBucketName, bucket)
	}

	var tags map[string]string
	if input.CreateBucketConfiguration != nil {
		var err error
		if tags, err = backend.ParseCreateBucketTags(input.CreateBucketConfiguration.Tags); err != nil {
			return err
		}
	}
	parsedACL, err := auth.ParseACL(acl)
	if err != nil {
		return err
	}

	obj := &simplyblockv1alpha2.S3Bucket{
		ObjectMeta: metav1.ObjectMeta{
			Name:        bucket,
			Namespace:   b.settings.Namespace,
			Annotations: map[string]string{simplyblockv1alpha2.S3BucketOwnerAnnotation: parsedACL.Owner},
		},
		Spec: simplyblockv1alpha2.S3BucketSpec{
			ObjectStoreRef:   b.settings.ObjectStore,
			BucketName:       bucket,
			CapacityBytes:    b.settings.CapacityBytes,
			StorageClassName: b.settings.StorageClassName,
		},
	}
	err = b.client.Create(ctx, obj)
	if apierrors.IsAlreadyExists(err) {
		return b.alreadyExists(ctx, bucket, parsedACL.Owner)
	}
	if apierrors.IsInvalid(err) {
		return s3err.GetBucketErr(s3err.ErrInvalidBucketName, bucket)
	}
	if err != nil {
		return fmt.Errorf("creating S3Bucket %s/%s: %w", obj.Namespace, obj.Name, err)
	}

	if err := b.provision(ctx, obj, input, acl, tags); err != nil {
		b.rollback(ctx, obj)
		return err
	}
	return nil
}

// provision waits for obj to become Ready and stores the request's bucket
// metadata on the mounted volume.
func (b *Backend) provision(
	ctx context.Context, obj *simplyblockv1alpha2.S3Bucket, input *s3.CreateBucketInput, acl []byte, tags map[string]string,
) error {
	bucket := obj.Spec.BucketName
	if err := b.waitReady(ctx, client.ObjectKeyFromObject(obj)); err != nil {
		return err
	}

	if err := b.PutBucketAcl(ctx, bucket, acl); err != nil {
		return err
	}
	if err := b.PutBucketOwnershipControls(ctx, bucket, input.ObjectOwnership); err != nil {
		return err
	}
	if tags != nil {
		if err := b.PutBucketTagging(ctx, bucket, tags); err != nil {
			return err
		}
	}
	if aws.ToBool(input.ObjectLockEnabledForBucket) {
		lock, err := json.Marshal(auth.BucketLockConfig{Enabled: true})
		if err != nil {
			return fmt.Errorf("marshalling the default bucket lock: %w", err)
		}
		if err := b.PutObjectLockConfiguration(ctx, bucket, lock); err != nil {
			return err
		}
	}
	return nil
}

// waitReady polls the S3Bucket until it is Ready, failing if it degrades,
// disappears, or Settings.Timeout passes first.
func (b *Backend) waitReady(ctx context.Context, key client.ObjectKey) error {
	err := wait.PollUntilContextTimeout(ctx, b.settings.PollInterval, b.settings.Timeout, true,
		func(ctx context.Context) (bool, error) {
			var obj simplyblockv1alpha2.S3Bucket
			if err := b.client.Get(ctx, key, &obj); err != nil {
				if apierrors.IsNotFound(err) {
					return false, internalError(fmt.Sprintf("S3Bucket %s was deleted while provisioning", key))
				}
				return false, fmt.Errorf("reading S3Bucket %s: %w", key, err)
			}
			switch obj.Status.Phase {
			case simplyblockv1alpha2.S3BucketPhaseReady:
				return true, nil
			case simplyblockv1alpha2.S3BucketPhaseDegraded:
				return false, internalError(fmt.Sprintf("S3Bucket %s is Degraded: %s", key, obj.Status.Message))
			default:
				return false, nil
			}
		})
	if wait.Interrupted(err) {
		return errServiceUnavailable
	}
	return err
}

// rollback deletes the S3Bucket a failed CreateBucket created. A failure is
// logged rather than returned: the caller is already failing with the error
// that matters.
func (b *Backend) rollback(ctx context.Context, obj *simplyblockv1alpha2.S3Bucket) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := b.client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		log.Printf("deleting S3Bucket %s/%s after a failed CreateBucket: %v", obj.Namespace, obj.Name, err)
	}
}

// alreadyExists answers a CreateBucket whose S3Bucket already exists:
// BucketAlreadyOwnedByYou when this gateway serves it and the same account
// created it, BucketAlreadyExists otherwise.
func (b *Backend) alreadyExists(ctx context.Context, bucket, owner string) error {
	var obj simplyblockv1alpha2.S3Bucket
	if err := b.client.Get(ctx, client.ObjectKey{Namespace: b.settings.Namespace, Name: bucket}, &obj); err != nil {
		return fmt.Errorf("reading existing S3Bucket %s: %w", bucket, err)
	}
	if obj.Spec.ObjectStoreRef == b.settings.ObjectStore &&
		obj.Annotations[simplyblockv1alpha2.S3BucketOwnerAnnotation] == owner {
		return s3err.GetBucketErr(s3err.ErrBucketAlreadyOwnedByYou, bucket)
	}
	return s3err.GetBucketErr(s3err.ErrBucketAlreadyExists, bucket)
}

// DeleteBucket deletes an empty bucket's S3Bucket and waits until it is
// gone, so the operator has withdrawn the mounts and released the volume
// before the client hears the bucket is deleted. A bucket still being
// created answers OperationAborted.
func (b *Backend) DeleteBucket(ctx context.Context, bucket string) error {
	key := client.ObjectKey{Namespace: b.settings.Namespace, Name: bucket}
	var obj simplyblockv1alpha2.S3Bucket
	if err := b.client.Get(ctx, key, &obj); err != nil {
		if apierrors.IsNotFound(err) {
			return s3err.GetBucketErr(s3err.ErrNoSuchBucket, bucket)
		}
		return fmt.Errorf("reading S3Bucket %s: %w", key, err)
	}
	if obj.Spec.ObjectStoreRef != b.settings.ObjectStore {
		return s3err.GetBucketErr(s3err.ErrNoSuchBucket, bucket)
	}

	if obj.DeletionTimestamp.IsZero() {
		switch obj.Status.Phase {
		case simplyblockv1alpha2.S3BucketPhaseReady:
			if err := b.ensureEmpty(bucket); err != nil {
				return err
			}
		case simplyblockv1alpha2.S3BucketPhaseDegraded:
			// Degraded is only reached before Ready, so the volume never
			// held objects.
		default:
			return errOperationAborted
		}
		err := b.client.Delete(ctx, &obj, client.Preconditions{UID: &obj.UID})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting S3Bucket %s: %w", key, err)
		}
	}

	err := wait.PollUntilContextTimeout(ctx, b.settings.PollInterval, b.settings.Timeout, true,
		func(ctx context.Context) (bool, error) {
			err := b.client.Get(ctx, key, &simplyblockv1alpha2.S3Bucket{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
	if wait.Interrupted(err) {
		return errServiceUnavailable
	}
	return err
}

// ensureEmpty is posix's own emptiness rule: anything in the bucket's root
// but the backend's scratch directory makes it non-empty.
func (b *Backend) ensureEmpty(bucket string) error {
	entries, err := os.ReadDir(filepath.Join(b.settings.RootDir, bucket))
	if errors.Is(err, os.ErrNotExist) {
		return s3err.GetBucketErr(s3err.ErrNoSuchBucket, bucket)
	}
	if err != nil {
		return fmt.Errorf("reading bucket %s: %w", bucket, err)
	}
	for _, entry := range entries {
		if entry.Name() != posix.MetaTmpDir {
			return s3err.GetBucketErr(s3err.ErrBucketNotEmpty, bucket)
		}
	}
	return nil
}

// internalError is an InternalError that carries why, for the client and
// the gateway's log.
func internalError(description string) s3err.APIError {
	err := s3err.GetAPIError(s3err.ErrInternalError)
	err.Description = description
	return err
}
