// Tests for the StoragePool conversion between v1alpha1 and the v1alpha2 hub.
//
// The pool is the kind the redesign regrouped rather than only renamed, so what
// is worth testing hardest is the three rules that make a regrouping safe:
//
//   - A group whose source is absent stays absent rather than becoming empty.
//     spec.volumeDefaults is immutable once set, so a pool handed an empty block
//     by the conversion could never be given a real one.
//   - The four QoS ceilings survive the string-to-integer change, including "0",
//     which means unlimited and is not the same as unset.
//   - The two removed spec fields survive a round trip through the annotation
//     they are stashed in, and leave no annotation behind on the way down.

package v1alpha1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestStoragePoolSpecRegroupsToTheHub(t *testing.T) {
	src := &StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-a", Namespace: "simplyblock"},
		Spec: StoragePoolSpec{
			ClusterName:          "production",
			CapacityLimit:        "10T",
			LogicalVolumeMaxSize: "2T",
			AllowedNodes:         []string{"worker-1", "worker-2"},
			DHCHAP:               true,
			QosSpec: &StoragePoolQoSSpec{
				IOPS: ptr.To(int32(200000)),
				Throughput: &StoragePoolQoSThroughputSpec{
					Read: ptr.To(int32(2048)), Write: ptr.To(int32(1024)), ReadWrite: ptr.To(int32(4096)),
				},
			},
			StorageClassParameters: &StorageClassParameters{
				QosRwIops:             "20000",
				QosRwMbytes:           "512",
				QosRMbytes:            "300",
				QosWMbytes:            "200",
				Encryption:            ptr.To(true),
				Fabric:                "tcp",
				MaxNamespacePerSubsys: "4",
				Tune2fsReservedBlocks: "1",
				Filesystem:            "xfs",
			},
		},
	}

	var hub v1alpha2.StoragePool
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	want := v1alpha2.StoragePoolSpec{
		ClusterRef:   "production",
		AllowedNodes: []string{"worker-1", "worker-2"},
		Limits: &v1alpha2.PoolLimits{
			Capacity:      "10T",
			MaxVolumeSize: "2T",
			IOPS:          ptr.To(int32(200000)),
			Throughput: &v1alpha2.ThroughputLimits{
				Read: ptr.To(int32(2048)), Write: ptr.To(int32(1024)), ReadWrite: ptr.To(int32(4096)),
			},
		},
		VolumeDefaults: &v1alpha2.VolumeDefaults{
			IOPS: ptr.To(int32(20000)),
			Throughput: &v1alpha2.ThroughputLimits{
				Read: ptr.To(int32(300)), Write: ptr.To(int32(200)), ReadWrite: ptr.To(int32(512)),
			},
			Filesystem:                "xfs",
			EnableEncryption:          ptr.To(true),
			EnableDHCHAP:              ptr.To(true),
			Fabric:                    "tcp",
			MaxNamespacesPerSubsystem: ptr.To(int32(4)),
			Tune2fsReservedBlocks:     "1",
		},
	}
	if diff := cmp.Diff(want, hub.Spec); diff != "" {
		t.Errorf("spec regrouped wrongly (-want +got):\n%s", diff)
	}
}

// The pool's own ceilings and its volumes' defaults are separate groups, which is
// the whole point of the regrouping. Nothing from one may appear in the other.
func TestStoragePoolLimitsAndVolumeDefaultsStaySeparate(t *testing.T) {
	src := &StoragePool{
		Spec: StoragePoolSpec{
			QosSpec:                &StoragePoolQoSSpec{IOPS: ptr.To(int32(200000))},
			StorageClassParameters: &StorageClassParameters{QosRwIops: "20000"},
		},
	}

	var hub v1alpha2.StoragePool
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if got := *hub.Spec.Limits.IOPS; got != 200000 {
		t.Errorf("spec.limits.iops = %d, want the pool's own ceiling 200000", got)
	}
	if got := *hub.Spec.VolumeDefaults.IOPS; got != 20000 {
		t.Errorf("spec.volumeDefaults.iops = %d, want each volume's default 20000", got)
	}
}

// A group with nothing to put in it stays absent. For spec.volumeDefaults this is
// not tidiness: the field is immutable once set, so an empty block written by the
// conversion is a value the user could never correct afterward.
func TestStoragePoolAbsentGroupsStayAbsent(t *testing.T) {
	src := &StoragePool{Spec: StoragePoolSpec{ClusterName: "production"}}

	var hub v1alpha2.StoragePool
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if hub.Spec.Limits != nil {
		t.Errorf("spec.limits = %+v, want nil", hub.Spec.Limits)
	}
	if hub.Spec.VolumeDefaults != nil {
		t.Errorf("spec.volumeDefaults = %+v, want nil", hub.Spec.VolumeDefaults)
	}
}

// "0" is the control plane's spelling of unlimited, so it has to survive as a
// ceiling of zero rather than collapsing into an absent one.
func TestStoragePoolZeroCeilingIsUnlimitedNotUnset(t *testing.T) {
	src := &StoragePool{
		Spec: StoragePoolSpec{
			StorageClassParameters: &StorageClassParameters{QosRwIops: "0", QosRwMbytes: "0"},
		},
	}

	var hub v1alpha2.StoragePool
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if hub.Spec.VolumeDefaults == nil || hub.Spec.VolumeDefaults.IOPS == nil {
		t.Fatalf("spec.volumeDefaults.iops is absent, want a ceiling of 0")
	}
	if got := *hub.Spec.VolumeDefaults.IOPS; got != 0 {
		t.Errorf("spec.volumeDefaults.iops = %d, want 0", got)
	}
	if hub.Spec.VolumeDefaults.Throughput == nil || hub.Spec.VolumeDefaults.Throughput.ReadWrite == nil {
		t.Fatalf("spec.volumeDefaults.throughput.readWrite is absent, want a ceiling of 0")
	}
}

// A ceiling this version stored that is not an integer converts to an absent one
// rather than failing the conversion, because a conversion webhook that errors
// makes the object unreadable rather than invalid.
func TestStoragePoolUnparsableCeilingBecomesAbsent(t *testing.T) {
	src := &StoragePool{
		Spec: StoragePoolSpec{
			StorageClassParameters: &StorageClassParameters{QosRwIops: "lots", Filesystem: "xfs"},
		},
	}

	var hub v1alpha2.StoragePool
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if hub.Spec.VolumeDefaults.IOPS != nil {
		t.Errorf("spec.volumeDefaults.iops = %d, want nil", *hub.Spec.VolumeDefaults.IOPS)
	}
	if hub.Spec.VolumeDefaults.Filesystem != "xfs" {
		t.Errorf("the rest of the block was lost: filesystem = %q", hub.Spec.VolumeDefaults.Filesystem)
	}
}

// The two fields the hub removed round-trip through the annotations they are
// stashed in, and the annotation is taken back out on the way down so that an
// object carries the field rather than both the field and the note about it.
func TestStoragePoolRemovedSpecFieldsRoundTrip(t *testing.T) {
	src := &StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-a"},
		Spec:       StoragePoolSpec{ClusterName: "production", Action: "rebalance", Status: "active"},
	}

	var hub v1alpha2.StoragePool
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if got := hub.Annotations[annoV1Alpha1PoolAction]; got != "rebalance" {
		t.Errorf("stashed action = %q, want %q", got, "rebalance")
	}

	var back StoragePool
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if back.Spec.Action != "rebalance" || back.Spec.Status != "active" {
		t.Errorf("spec.action = %q, spec.status = %q, want rebalance and active",
			back.Spec.Action, back.Spec.Status)
	}
	if _, ok := back.Annotations[annoV1Alpha1PoolAction]; ok {
		t.Errorf("the stash annotation survived the trip down: %v", back.Annotations)
	}
}

// A pool that set neither removed field is not given metadata it never had.
func TestStoragePoolUnsetRemovedFieldsWriteNoAnnotation(t *testing.T) {
	src := &StoragePool{Spec: StoragePoolSpec{ClusterName: "production"}}

	var hub v1alpha2.StoragePool
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if len(hub.Annotations) != 0 {
		t.Errorf("annotations = %v, want none", hub.Annotations)
	}
}

// Stashing must not write through to the object being converted. The conversion
// webhook is handed the live object, and an annotation added to it is an edit
// nobody asked for.
func TestStoragePoolStashDoesNotMutateTheSource(t *testing.T) {
	src := &StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-a"},
		Spec:       StoragePoolSpec{ClusterName: "production", Action: "rebalance"},
	}

	var hub v1alpha2.StoragePool
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if len(src.Annotations) != 0 {
		t.Errorf("the source gained annotations: %v", src.Annotations)
	}
}

func TestStoragePoolStatusRenamesQoSToLimits(t *testing.T) {
	src := &StoragePool{
		Status: StoragePoolStatus{
			UUID:   "4f2c8a11-6b3d-4e19-9a55-0c7e1d8f2b34",
			Status: "online",
			QoS: &StoragePoolQoSStatus{
				Host: "node-a",
				IOPS: ptr.To(int32(200000)),
				Throughput: &StoragePoolQoSThroughputStatus{
					Read: ptr.To(int32(2048)), Write: ptr.To(int32(1024)), ReadWrite: ptr.To(int32(4096)),
				},
			},
			AllowedNodes: []string{"worker-1"},
		},
	}

	var hub v1alpha2.StoragePool
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	want := &v1alpha2.PoolLimitsStatus{
		Host: "node-a",
		IOPS: ptr.To(int32(200000)),
		Throughput: &v1alpha2.ThroughputLimits{
			Read: ptr.To(int32(2048)), Write: ptr.To(int32(1024)), ReadWrite: ptr.To(int32(4096)),
		},
	}
	if diff := cmp.Diff(want, hub.Status.Limits); diff != "" {
		t.Errorf("status.limits (-want +got):\n%s", diff)
	}
}
