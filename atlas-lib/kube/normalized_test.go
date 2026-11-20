package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/lvol"
)

const (
	testCluster  = "8ffac363-0c46-4714-a71b-f9c0b58a1269"
	testPoolUUID = "df34f16c-1a2b-3c4d-5e6f-7a8b9c0d1e2f"
	testVolume   = "a1111111-1111-4111-8111-111111111111"

	testLegacyHandle     = testCluster + ":production:" + testVolume
	testNormalizedHandle = testCluster + ":" + testPoolUUID + ":" + testVolume
)

func legacyPV(annotations map[string]string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1", Annotations: annotations},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       DriverName,
					VolumeHandle: testLegacyHandle,
				},
			},
		},
	}
}

// A PersistentVolume that has been through the migration reports the pool it
// resolves to, and the same object before the migration reports the name its
// field carries. Both are correct answers to different questions, and this is
// the one that asks which pool the volume is actually in.
func TestNormalizedVolumeHandleFromPV(t *testing.T) {
	before, err := NormalizedVolumeHandleFromPV(legacyPV(nil))
	if err != nil {
		t.Fatalf("reading an unmigrated volume: %v", err)
	}
	if before.Handle.PoolRef != "production" || before.FromAnnotation {
		t.Errorf("an unmigrated volume reported %+v, want the field's pool name", before)
	}

	after, err := NormalizedVolumeHandleFromPV(legacyPV(map[string]string{
		AnnoVolumeHandle: testNormalizedHandle,
	}))
	if err != nil {
		t.Fatalf("reading a migrated volume: %v", err)
	}
	if after.Handle.PoolRef != testPoolUUID || !after.FromAnnotation {
		t.Errorf("a migrated volume reported %+v, want the annotated pool UUID", after)
	}
}

// An annotation that disagrees about anything but the pool is ignored, and the
// object keeps reporting what its own spec says. Whoever may edit metadata must
// not be able to point a volume at another cluster.
func TestNormalizedVolumeHandleFromPVIgnoresARedirectingAnnotation(t *testing.T) {
	const elsewhere = "11111111-2222-4333-8444-555555555555:" + testPoolUUID + ":" + testVolume

	got, err := NormalizedVolumeHandleFromPV(legacyPV(map[string]string{
		AnnoVolumeHandle: elsewhere,
	}))
	if err != nil {
		t.Fatalf("reading the volume: %v", err)
	}
	if got.Handle.ClusterID != testCluster {
		t.Errorf("cluster = %s, want the field's %s: an annotation redirected the volume",
			got.Handle.ClusterID, testCluster)
	}
	if got.Ignored == "" {
		t.Error("the annotation was ignored and nothing says so, so a hand-edited " +
			"annotation leaves no trace for anybody to find")
	}
}

// A volume another driver owns has no simplyblock handle to normalize, and
// saying so is the same answer VolumeHandleFromPV gives.
func TestNormalizedVolumeHandleFromPVRefusesAForeignVolume(t *testing.T) {
	pv := legacyPV(nil)
	pv.Spec.CSI.Driver = "ebs.csi.aws.com"
	if _, err := NormalizedVolumeHandleFromPV(pv); err == nil {
		t.Error("a volume owned by another driver was read as a simplyblock one")
	}
}

// The annotation-reading half takes a map rather than an object, so a
// VolumeSnapshotContent gets the rule without this module depending on the
// snapshot API.
func TestNormalizedHandleReadsAnyObjectsAnnotations(t *testing.T) {
	got, ok := NormalizedHandle(
		lvol.VolumeHandle(testLegacyHandle),
		map[string]string{AnnoVolumeHandle: testNormalizedHandle},
	)
	if !ok {
		t.Fatal("a well-formed handle was not read")
	}
	if got.Handle.PoolRef != testPoolUUID {
		t.Errorf("pool = %q, want the annotated %q", got.Handle.PoolRef, testPoolUUID)
	}
}
