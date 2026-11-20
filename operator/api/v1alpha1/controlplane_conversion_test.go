// Tests for the ControlPlane conversion between v1alpha1 and the v1alpha2 hub.
//
// Two properties are converted (design-property-renames.md §2.4 and §2.5): the
// top-level image regroups under spec.source.local, and the readiness phase
// Ready becomes Available. Both directions are tested, because a conversion that
// renames going up and copies going down corrupts on the first
// `kubectl get -o yaml | kubectl apply -f -` and a one-way test cannot see it.

package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// testSystemVolumeFilter is the drain's system-volume pattern, shared by the
// conversion tests that carry a RemoveSpec through a round trip.
const testSystemVolumeFilter = "^system-.*"

const testImage = "quay.io/simplyblock-io/simplyblock:26.2.2"

// testCluster is the cluster name every conversion fixture names, so the
// literal appears once rather than in each of them.
const testCluster = "production"

func TestControlPlaneConvertToRegroupsImage(t *testing.T) {
	src := &ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock", Namespace: "sb"},
		Spec:       ControlPlaneSpec{Image: testImage},
	}

	var dst v1alpha2.ControlPlane
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if dst.Spec.Source.Local == nil {
		t.Fatalf("spec.source.local is absent, want the image regrouped under it")
	}
	if got := dst.Spec.Source.Local.Image; got != testImage {
		t.Errorf("spec.source.local.image = %q, want %q", got, testImage)
	}
	if dst.Name != "simplyblock" || dst.Namespace != "sb" {
		t.Errorf("object meta not carried: %q/%q", dst.Namespace, dst.Name)
	}
}

// Every v1alpha1 ControlPlane is one the chart installed, so an absent image
// still converts into a local source. The hub requires exactly one member of
// spec.source, and the reconciler classifies an object by which member is set.
func TestControlPlaneConvertToStillNamesManagedWhenImageEmpty(t *testing.T) {
	src := &ControlPlane{Spec: ControlPlaneSpec{Image: ""}}

	var dst v1alpha2.ControlPlane
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if dst.Spec.Source.Local == nil {
		t.Fatalf("spec.source.local is absent, want a managed source with an empty image")
	}
	if got := dst.Spec.Source.Local.Image; got != "" {
		t.Errorf("spec.source.local.image = %q, want empty", got)
	}
	if dst.Spec.Source.Managed != nil {
		t.Errorf("spec.source.managed = %+v, want nil", dst.Spec.Source.Managed)
	}
}

func TestControlPlaneConvertToRenamesReadyPhase(t *testing.T) {
	for _, tc := range []struct {
		name string
		from string
		want string
	}{
		{"ready becomes available", "Ready", "Available"},
		{"initializing becomes installing", "Initializing", "Installing"},
		{"empty is unchanged", "", ""},
		{"an unrecognized value passes through", "Wedged", "Wedged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &ControlPlane{Status: ControlPlaneStatus{Phase: tc.from}}

			var dst v1alpha2.ControlPlane
			if err := src.ConvertTo(&dst); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			if got := string(dst.Status.Phase); got != tc.want {
				t.Errorf("status.phase = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestControlPlaneConvertFromUngroupsImage(t *testing.T) {
	src := &v1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock", Namespace: "sb"},
		Spec: v1alpha2.ControlPlaneSpec{
			Source: v1alpha2.ControlPlaneSource{
				Local: &v1alpha2.LocalControlPlane{Image: testImage},
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

// Every hub phase has to land on a value this version's Enum admits, which is
// Initializing or Ready. Degraded and Unavailable have no v1alpha1 spelling, so
// they map onto the one that describes the same thing to a v1alpha1 reader:
// Degraded answers requests, and Unavailable does not.
func TestControlPlaneConvertFromMapsEveryHubPhase(t *testing.T) {
	for _, tc := range []struct {
		from v1alpha2.ControlPlanePhase
		want string
	}{
		{"Available", "Ready"},
		{"Degraded", "Ready"},
		{"Installing", "Initializing"},
		{"Unavailable", "Initializing"},
		{"", ""},
	} {
		t.Run(string(tc.from), func(t *testing.T) {
			src := &v1alpha2.ControlPlane{
				Status: v1alpha2.ControlPlaneStatus{Phase: tc.from},
			}

			var dst ControlPlane
			if err := dst.ConvertFrom(src); err != nil {
				t.Fatalf("ConvertFrom: %v", err)
			}
			if got := dst.Status.Phase; got != tc.want {
				t.Errorf("status.phase = %q, want %q", got, tc.want)
			}
		})
	}
}

// Both directions have to produce a value the target version's Enum admits. A
// phase outside it is an object the API server rejects on write and the new
// controller cannot interpret, which is how a migration stops on a control
// plane that was merely starting up.
func TestControlPlanePhasesStayInsideBothEnums(t *testing.T) {
	hubPhases := map[string]bool{
		"Installing": true, "Available": true, "Degraded": true, "Unavailable": true,
	}
	spokePhases := map[string]bool{"Initializing": true, "Ready": true}

	for phase := range spokePhases {
		var hub v1alpha2.ControlPlane
		src := &ControlPlane{Status: ControlPlaneStatus{Phase: phase}}
		if err := src.ConvertTo(&hub); err != nil {
			t.Fatalf("ConvertTo: %v", err)
		}
		if !hubPhases[string(hub.Status.Phase)] {
			t.Errorf("%q converts up to %q, which v1alpha2's Enum does not admit",
				phase, hub.Status.Phase)
		}
	}

	for phase := range hubPhases {
		var spoke ControlPlane
		src := &v1alpha2.ControlPlane{
			Status: v1alpha2.ControlPlaneStatus{Phase: v1alpha2.ControlPlanePhase(phase)},
		}
		if err := spoke.ConvertFrom(src); err != nil {
			t.Fatalf("ConvertFrom: %v", err)
		}
		if !spokePhases[spoke.Status.Phase] {
			t.Errorf("%q converts down to %q, which v1alpha1's Enum does not admit",
				phase, spoke.Status.Phase)
		}
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

// A v1alpha1 ControlPlane has no TLS field, because the installation's TLS was
// stated to the operator through its environment. Reading it as the hub's
// default, which is on, flips a plaintext installation to TLS on its first
// write, and spec.source.local.tls is immutable from then on. So an object that
// states nothing converts to plaintext, which is what the release that wrote it
// installed by default, and it is explicit so that nothing defaults it later.
func TestControlPlaneConvertToIsPlaintextWhenNoTLSWasRecorded(t *testing.T) {
	src := &ControlPlane{Spec: ControlPlaneSpec{Image: testImage}}

	var dst v1alpha2.ControlPlane
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	tls := dst.Spec.Source.Local.TLS
	if tls.EnableTLS == nil || *tls.EnableTLS {
		t.Errorf("enableTLS = %v, want an explicit false", tls.EnableTLS)
	}
	if tls.EnableMutualTLS == nil || *tls.EnableMutualTLS {
		t.Errorf("enableMutualTLS = %v, want an explicit false", tls.EnableMutualTLS)
	}
	if tls.Provider != v1alpha2.ControlPlaneTLSCertManager {
		t.Errorf("provider = %q, want %q", tls.Provider, v1alpha2.ControlPlaneTLSCertManager)
	}
}

// An installation that did use TLS records it on the object before the upgrade,
// and conversion carries it. The annotation is consumed, so the converted object
// carries the field rather than both the field and the note about it.
func TestControlPlaneConvertToCarriesTheRecordedTLS(t *testing.T) {
	src := &ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationControlPlaneTLS: `{"enableTLS":true,"enableMutualTLS":false,"provider":"OpenShift"}`,
				"unrelated":               "kept",
			},
		},
		Spec: ControlPlaneSpec{Image: testImage},
	}

	var dst v1alpha2.ControlPlane
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	tls := dst.Spec.Source.Local.TLS
	if tls.EnableTLS == nil || !*tls.EnableTLS {
		t.Errorf("enableTLS = %v, want true", tls.EnableTLS)
	}
	if tls.EnableMutualTLS == nil || *tls.EnableMutualTLS {
		t.Errorf("enableMutualTLS = %v, want an explicit false", tls.EnableMutualTLS)
	}
	if tls.Provider != v1alpha2.ControlPlaneTLSOpenShift {
		t.Errorf("provider = %q, want %q", tls.Provider, v1alpha2.ControlPlaneTLSOpenShift)
	}
	if _, left := dst.Annotations[AnnotationControlPlaneTLS]; left {
		t.Error("the TLS annotation was left on the converted object")
	}
	if dst.Annotations["unrelated"] != "kept" {
		t.Error("an unrelated annotation was lost")
	}
	if _, left := src.Annotations[AnnotationControlPlaneTLS]; !left {
		t.Error("conversion consumed the annotation on the object it was given")
	}
}

// A record that cannot be read costs the setting rather than the object, as every
// other stash does, and what is left is the plaintext an unrecorded object gets.
func TestControlPlaneConvertToIgnoresAnUnreadableTLSRecord(t *testing.T) {
	src := &ControlPlane{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{AnnotationControlPlaneTLS: "not json"},
	}}

	var dst v1alpha2.ControlPlane
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if tls := dst.Spec.Source.Local.TLS; tls.EnableTLS == nil || *tls.EnableTLS {
		t.Errorf("enableTLS = %v, want an explicit false", tls.EnableTLS)
	}
}

// The API server converts a v1alpha2 object down for a v1alpha1 reader and back
// up when that reader writes. The old operator is such a reader during an
// upgrade, so a v1alpha1 view that dropped the TLS would reset it on its next
// write, and the field is immutable.
func TestControlPlaneTLSSurvivesTheV1Alpha1View(t *testing.T) {
	hub := &v1alpha2.ControlPlane{Spec: v1alpha2.ControlPlaneSpec{
		Source: v1alpha2.ControlPlaneSource{Local: &v1alpha2.LocalControlPlane{
			Image: testImage,
			TLS: v1alpha2.ControlPlaneTLS{
				EnableTLS:       ptr.To(true),
				EnableMutualTLS: ptr.To(false),
				Provider:        v1alpha2.ControlPlaneTLSOpenShift,
			},
		}},
	}}

	var spoke ControlPlane
	if err := spoke.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if spoke.Annotations[AnnotationControlPlaneTLS] == "" {
		t.Fatal("the v1alpha1 view carries no record of the TLS")
	}

	var back v1alpha2.ControlPlane
	if err := spoke.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	got, want := back.Spec.Source.Local.TLS, hub.Spec.Source.Local.TLS
	if *got.EnableTLS != *want.EnableTLS || *got.EnableMutualTLS != *want.EnableMutualTLS ||
		got.Provider != want.Provider {
		t.Errorf("TLS after the round trip = %+v, want %+v", got, want)
	}
	if _, left := hub.Annotations[AnnotationControlPlaneTLS]; left {
		t.Error("conversion wrote its record onto the hub object it was given")
	}
}
