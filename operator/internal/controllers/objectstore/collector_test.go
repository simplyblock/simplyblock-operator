// Tests for the bucket phase collector: the gauge it publishes from the
// bucket objects that exist right now, asserted through client_golang's
// testutil rather than by scraping, since what matters is the value under a
// label set and not the exposition format.

package objectstore

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
)

func collectorBucket(name string, phase objectstorev1alpha1.S3BucketPhase) *objectstorev1alpha1.S3Bucket {
	return &objectstorev1alpha1.S3Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testBucketNamespace},
		Spec:       objectstorev1alpha1.S3BucketSpec{BucketName: name, ObjectStoreRef: testObjectStoreName},
		Status:     objectstorev1alpha1.S3BucketStatus{Phase: phase},
	}
}

// U-20: 1 for the bucket's current phase, 0 for the others.
func TestCollectorPublishesOneForTheCurrentPhase(t *testing.T) {
	cli := newTestClient(t, collectorBucket("collector-bucket-ready", phaseReady))
	c := &Collector{Client: cli}

	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}

	for _, phase := range bucketPhases {
		want := 0.0
		if phase == phaseReady {
			want = 1
		}
		got := testutil.ToFloat64(bucketPhaseState.WithLabelValues("collector-bucket-ready", testObjectStoreName, string(phase)))
		if got != want {
			t.Errorf("phase %q = %v, want %v", phase, got, want)
		}
	}
}

// U-21: a bucket that no longer exists is cleared rather than left reporting
// its last phase forever: the reset-then-rebuild shape every pass takes.
func TestCollectorClearsADeletedBucketsSeries(t *testing.T) {
	bucket := collectorBucket("collector-bucket-deleted", phaseProvisioning)
	cli := newTestClient(t, bucket)
	c := &Collector{Client: cli}
	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := testutil.ToFloat64(bucketPhaseState.WithLabelValues("collector-bucket-deleted", testObjectStoreName, string(phaseProvisioning))); got != 1 {
		t.Fatalf("phase before delete = %v, want 1", got)
	}

	if err := cli.Delete(context.Background(), bucket); err != nil {
		t.Fatalf("deleting the bucket: %v", err)
	}
	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect after delete: %v", err)
	}
	if got := testutil.ToFloat64(bucketPhaseState.WithLabelValues("collector-bucket-deleted", testObjectStoreName, string(phaseProvisioning))); got != 0 {
		t.Errorf("phase after delete = %v, want 0 (series cleared)", got)
	}
}
