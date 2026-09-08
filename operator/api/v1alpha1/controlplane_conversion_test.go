// Tests for the ControlPlane conversion between v1alpha1 and the v1alpha2 hub.
//
// Two properties are converted (design-property-renames.md §2.4 and §2.5): the
// top-level image regroups under spec.source.managed, and the readiness phase
// Ready becomes Available. Both directions are tested, because a conversion that
// renames going up and copies going down corrupts on the first
// `kubectl get -o yaml | kubectl apply -f -` and a one-way test cannot see it.

package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const testImage = "quay.io/simplyblock-io/simplyblock:26.2.2"

func TestControlPlaneConvertToRegroupsImage(t *testing.T) {
	src := &ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock", Namespace: "sb"},
		Spec:       ControlPlaneSpec{Image: testImage},
	}

	var dst v1alpha2.ControlPlane
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if dst.Spec.Source == nil || dst.Spec.Source.Managed == nil {
		t.Fatalf("spec.source.managed is absent, want the image regrouped under it")
	}
	if got := dst.Spec.Source.Managed.Image; got != testImage {
		t.Errorf("spec.source.managed.image = %q, want %q", got, testImage)
	}
	if dst.Name != "simplyblock" || dst.Namespace != "sb" {
		t.Errorf("object meta not carried: %q/%q", dst.Namespace, dst.Name)
	}
}

// An absent image must leave spec.source absent rather than allocating an empty
// struct. A conversion that writes an empty parent hands the user a value they
// did not set, which for the immutable groups elsewhere in this migration cannot
// then be corrected.
func TestControlPlaneConvertToLeavesSourceAbsentWhenImageEmpty(t *testing.T) {
	src := &ControlPlane{Spec: ControlPlaneSpec{Image: ""}}

	var dst v1alpha2.ControlPlane
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if dst.Spec.Source != nil {
		t.Errorf("spec.source = %+v, want nil for an unset image", dst.Spec.Source)
	}
}

func TestControlPlaneConvertToRenamesReadyPhase(t *testing.T) {
	for _, tc := range []struct {
		name string
		from string
		want string
	}{
		{"ready becomes available", "Ready", "Available"},
		{"initializing is unchanged", "Initializing", "Initializing"},
		{"empty is unchanged", "", ""},
		{"an unrecognized value passes through", "Wedged", "Wedged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &ControlPlane{Status: ControlPlaneStatus{Phase: tc.from}}

			var dst v1alpha2.ControlPlane
			if err := src.ConvertTo(&dst); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			if got := dst.Status.Phase; got != tc.want {
				t.Errorf("status.phase = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestControlPlaneConvertFromUngroupsImage(t *testing.T) {
	src := &v1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock", Namespace: "sb"},
		Spec: v1alpha2.ControlPlaneSpec{
			Source: &v1alpha2.ControlPlaneSource{
				Managed: &v1alpha2.ManagedControlPlane{Image: testImage},
			},
		},
	}

	var dst ControlPlane
	if err := dst.ConvertFrom(src); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	if got := dst.Spec.Image; got != testImage {
		t.Errorf("spec.image = %q, want %q", got, testImage)
	}
	if dst.Name != "simplyblock" || dst.Namespace != "sb" {
		t.Errorf("object meta not carried: %q/%q", dst.Namespace, dst.Name)
	}
}

func TestControlPlaneConvertFromRenamesAvailablePhase(t *testing.T) {
	src := &v1alpha2.ControlPlane{
		Status: v1alpha2.ControlPlaneStatus{Phase: "Available"},
	}

	var dst ControlPlane
	if err := dst.ConvertFrom(src); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if got := dst.Status.Phase; got != "Ready" {
		t.Errorf("status.phase = %q, want %q", got, "Ready")
	}
}

// A v1alpha1 object that goes up to the hub and back must come back unchanged.
// This is what the API server does on every read of a stored v1alpha1 object, so
// a conversion that loses a field here loses it on the cluster.
func TestControlPlaneRoundTripsThroughTheHub(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  *ControlPlane
	}{
		{
			name: "an image and a ready phase",
			obj: &ControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "simplyblock", Namespace: "sb"},
				Spec:       ControlPlaneSpec{Image: testImage},
				Status:     ControlPlaneStatus{Phase: "Ready", Message: "healthy"},
			},
		},
		{
			name: "no image and an initializing phase",
			obj: &ControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "simplyblock"},
				Status:     ControlPlaneStatus{Phase: "Initializing", Message: "fdb unavailable"},
			},
		},
		{
			name: "nothing set at all",
			obj:  &ControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "simplyblock"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hub v1alpha2.ControlPlane
			if err := tc.obj.ConvertTo(&hub); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}

			var back ControlPlane
			if err := back.ConvertFrom(&hub); err != nil {
				t.Fatalf("ConvertFrom: %v", err)
			}

			if back.Spec.Image != tc.obj.Spec.Image {
				t.Errorf("spec.image = %q, want %q", back.Spec.Image, tc.obj.Spec.Image)
			}
			if back.Status.Phase != tc.obj.Status.Phase {
				t.Errorf("status.phase = %q, want %q", back.Status.Phase, tc.obj.Status.Phase)
			}
			if back.Status.Message != tc.obj.Status.Message {
				t.Errorf("status.message = %q, want %q", back.Status.Message, tc.obj.Status.Message)
			}
			if back.Name != tc.obj.Name || back.Namespace != tc.obj.Namespace {
				t.Errorf("object meta = %q/%q, want %q/%q",
					back.Namespace, back.Name, tc.obj.Namespace, tc.obj.Name)
			}
		})
	}
}
