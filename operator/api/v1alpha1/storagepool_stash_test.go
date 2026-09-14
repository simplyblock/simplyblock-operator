// Tests for the half of the StoragePool conversion that carries what v1alpha1
// cannot express.
//
// design-api-upgrade.md §6.2 requires that nothing without a counterpart on both
// sides silently disappears, and while v1alpha1 is the storage version the cost
// of getting that wrong is not an upgrade-time one: every write of a v1alpha2
// object is stored as v1alpha1 and read back, so a field that does not survive
// is lost on each of them. A controller writing status.phase would read back a
// pool that had never had a phase.

package v1alpha1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Every hub field with no home here survives being stored and read back.
func TestStoragePoolHubOnlyFieldsAreStashed(t *testing.T) {
	hub := &v1alpha2.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-a", Namespace: "simplyblock"},
		Spec: v1alpha2.StoragePoolSpec{
			ClusterRef: "production",
			VolumeDefaults: &v1alpha2.VolumeDefaults{
				EnableCompression: ptr.To(true),
				EnableReplication: ptr.To(false),
				PriorityClass:     "high",
			},
		},
		Status: v1alpha2.StoragePoolStatus{
			Phase:                   v1alpha2.StoragePoolPhaseReady,
			StorageClassNames:       []string{"archive-ext4", "fast-xfs"},
			DefaultStorageClassName: "simplyblock-production",
			ActiveOpsRef:            "rebalance-1",
			Message:                 "ready",
			ObservedGeneration:      3,
		},
	}

	var stored StoragePool
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	// EnableReplication is false rather than absent, and a conversion that read
	// a false pointer as nothing would drop it.
	for _, key := range []string{
		annoEnableCompression, annoEnableReplication, annoPriorityClass,
		annoStatusPhase, annoStatusClassNames, annoStatusDefaultClassName,
		annoStatusActiveOpsRef, annoStatusMessage, annoStatusObservedGeneraton,
	} {
		if _, ok := stored.Annotations[key]; !ok {
			t.Errorf("nothing was stashed under %q, so the field is lost on this write", key)
		}
	}

	var back v1alpha2.StoragePool
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if diff := cmp.Diff(hub.Status, back.Status); diff != "" {
		t.Errorf("the status did not survive the trip (-written +read):\n%s", diff)
	}
	if diff := cmp.Diff(hub.Spec.VolumeDefaults, back.Spec.VolumeDefaults); diff != "" {
		t.Errorf("the volume defaults did not survive the trip (-written +read):\n%s", diff)
	}
}

// The annotations are conversion state, so an object read as v1alpha2 carries
// the fields rather than both the fields and the notes about them.
func TestStoragePoolStashIsRemovedOnTheWayUp(t *testing.T) {
	hub := &v1alpha2.StoragePool{
		Spec:   v1alpha2.StoragePoolSpec{ClusterRef: "production"},
		Status: v1alpha2.StoragePoolStatus{Phase: v1alpha2.StoragePoolPhaseReady},
	}

	var stored StoragePool
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StoragePool
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if len(back.Annotations) != 0 {
		t.Errorf("annotations = %v, want the conversion state taken back out", back.Annotations)
	}
}

// A pool that states none of them gains no metadata. Nine keys on every object
// that set nothing would be conversion state masquerading as a user's.
func TestStoragePoolStashesNothingForAnEmptyHub(t *testing.T) {
	hub := &v1alpha2.StoragePool{Spec: v1alpha2.StoragePoolSpec{ClusterRef: "production"}}

	var stored StoragePool
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	if len(stored.Annotations) != 0 {
		t.Errorf("annotations = %v, want none", stored.Annotations)
	}
}

// A value the hub stops stating is cleared rather than left behind. A stale
// stash would restore a default the hub no longer carries, which is the failure
// mode a write-only stash has and a rewritten one does not.
func TestStoragePoolStaleStashIsCleared(t *testing.T) {
	withDefaults := &v1alpha2.StoragePool{
		Spec: v1alpha2.StoragePoolSpec{
			ClusterRef:     "production",
			VolumeDefaults: &v1alpha2.VolumeDefaults{PriorityClass: "high"},
		},
		Status: v1alpha2.StoragePoolStatus{Message: "ready"},
	}

	var stored StoragePool
	if err := stored.ConvertFrom(withDefaults); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if _, ok := stored.Annotations[annoPriorityClass]; !ok {
		t.Fatal("the priority class was not stashed to begin with")
	}

	// The same object, rewritten with neither value.
	cleared := &v1alpha2.StoragePool{
		ObjectMeta: *stored.ObjectMeta.DeepCopy(),
		Spec:       v1alpha2.StoragePoolSpec{ClusterRef: "production"},
	}
	var rewritten StoragePool
	if err := rewritten.ConvertFrom(cleared); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	if _, ok := rewritten.Annotations[annoPriorityClass]; ok {
		t.Error("the stashed priority class outlived the block it came from")
	}
	if _, ok := rewritten.Annotations[annoStatusMessage]; ok {
		t.Error("the stashed message outlived the status that carried it")
	}
}

// A hand-edited annotation costs the field it names and nothing else. Refusing
// to convert would take the whole object out of reach, which is worse than
// losing the one value somebody corrupted.
func TestStoragePoolAnUndecodableStashIsDropped(t *testing.T) {
	stored := StoragePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tenant-a",
			Annotations: map[string]string{
				annoStatusObservedGeneraton: "not a number",
				annoStatusMessage:           `"ready"`,
			},
		},
	}

	var hub v1alpha2.StoragePool
	if err := stored.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if hub.Status.ObservedGeneration != 0 {
		t.Errorf("status.observedGeneration = %d, want 0", hub.Status.ObservedGeneration)
	}
	if hub.Status.Message != "ready" {
		t.Errorf("status.message = %q, want the field beside the broken one intact", hub.Status.Message)
	}
}

// U-114: an explicit enableDHCHAP of false survives.
//
// v1alpha1 has a field for the toggle, but a bool rather than a pointer, so it
// can say true and it cannot tell false from absent. Without a stash the false
// comes back nil — and on a pool whose defaults are nothing else it takes the
// whole spec.volumeDefaults block with it, a block that is immutable once set
// and so could never be restored.
func TestStoragePoolExplicitFalseDHCHAPSurvives(t *testing.T) {
	hub := &v1alpha2.StoragePool{
		Spec: v1alpha2.StoragePoolSpec{
			ClusterRef:     "production",
			VolumeDefaults: &v1alpha2.VolumeDefaults{EnableDHCHAP: ptr.To(false)},
		},
	}

	var stored StoragePool
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StoragePool
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if back.Spec.VolumeDefaults == nil {
		t.Fatal("spec.volumeDefaults is nil, so an immutable block was lost on one write")
	}
	if back.Spec.VolumeDefaults.EnableDHCHAP == nil {
		t.Fatal("enableDHCHAP came back absent, not false")
	}
	if *back.Spec.VolumeDefaults.EnableDHCHAP {
		t.Error("enableDHCHAP came back true, inverting what was written")
	}
}

// A real v1alpha1 client writing dhchap: true carries no stash, and the toggle
// still converts up from the field it does have.
func TestStoragePoolDHCHAPFromAV1Alpha1Client(t *testing.T) {
	stored := StoragePool{Spec: StoragePoolSpec{ClusterName: "production", DHCHAP: true}}

	var hub v1alpha2.StoragePool
	if err := stored.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if hub.Spec.VolumeDefaults == nil || hub.Spec.VolumeDefaults.EnableDHCHAP == nil {
		t.Fatal("an object a v1alpha1 client wrote lost its dhchap toggle")
	}
	if !*hub.Spec.VolumeDefaults.EnableDHCHAP {
		t.Error("dhchap: true converted to enableDHCHAP: false")
	}
}

// An annotation a user wrote is left alone. The conversion owns its own keys and
// nothing else on the object.
func TestStoragePoolStashLeavesOtherAnnotationsAlone(t *testing.T) {
	hub := &v1alpha2.StoragePool{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"team.example.com/owner": "storage"},
		},
		Spec: v1alpha2.StoragePoolSpec{ClusterRef: "production"},
	}

	var stored StoragePool
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StoragePool
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if back.Annotations["team.example.com/owner"] != "storage" {
		t.Errorf("annotations = %v, want the user's own left in place", back.Annotations)
	}
}
