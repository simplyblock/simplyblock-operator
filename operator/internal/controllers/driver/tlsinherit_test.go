// Tests for the driver's TLS following the ControlPlane in its namespace where
// the driver states none of its own.

package driver

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/controlplane"
)

func localCP(tls simplyblockv1alpha2.ControlPlaneTLS) *simplyblockv1alpha2.ControlPlane {
	return &simplyblockv1alpha2.ControlPlane{Spec: simplyblockv1alpha2.ControlPlaneSpec{
		Source: simplyblockv1alpha2.ControlPlaneSource{
			Local: &simplyblockv1alpha2.LocalControlPlane{Image: "quay.io/simplyblock-io/simplyblock:1", TLS: tls},
		},
	}}
}

func TestAnUnsetDriverTLSFollowsTheControlPlane(t *testing.T) {
	for name, tc := range map[string]struct {
		cp                  *simplyblockv1alpha2.ControlPlane
		wantTLS, wantMutual bool
		wantProvider        simplyblockv1alpha2.DriverTLSProvider
	}{
		"control plane defaults: mutual TLS through cert-manager": {
			cp: localCP(simplyblockv1alpha2.ControlPlaneTLS{}), wantTLS: true, wantMutual: true,
			wantProvider: simplyblockv1alpha2.DriverTLSProviderCertManager,
		},
		"control plane without TLS": {
			cp:           localCP(simplyblockv1alpha2.ControlPlaneTLS{EnableTLS: ptr.To(false)}),
			wantProvider: simplyblockv1alpha2.DriverTLSProviderCertManager,
		},
		"one-way TLS through the OpenShift service CA": {
			cp: localCP(simplyblockv1alpha2.ControlPlaneTLS{
				EnableMutualTLS: ptr.To(false), Provider: simplyblockv1alpha2.ControlPlaneTLSOpenShift,
			}),
			wantTLS: true, wantProvider: simplyblockv1alpha2.DriverTLSProviderOpenShift,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := inheritTLS(simplyblockv1alpha2.DriverTLS{}, tc.cp)
			if *got.EnableTLS != tc.wantTLS || *got.EnableMutualTLS != tc.wantMutual || got.Provider != tc.wantProvider {
				t.Errorf("got enableTLS=%v enableMutualTLS=%v provider=%q, want %v %v %q",
					*got.EnableTLS, *got.EnableMutualTLS, got.Provider, tc.wantTLS, tc.wantMutual, tc.wantProvider)
			}
		})
	}
}

// A value the driver states is kept, whatever the control plane runs.
func TestAStatedDriverTLSWins(t *testing.T) {
	stated := simplyblockv1alpha2.DriverTLS{EnableTLS: ptr.To(false)}
	got := inheritTLS(stated, localCP(simplyblockv1alpha2.ControlPlaneTLS{}))
	if *got.EnableTLS {
		t.Error("the driver's explicit enableTLS=false was overridden")
	}
}

// Without a ControlPlane to follow, or with one this cluster does not host, an
// unset field is off, which is what the driver did before.
func TestWithoutALocalControlPlaneUnsetTLSIsOff(t *testing.T) {
	for name, cp := range map[string]*simplyblockv1alpha2.ControlPlane{
		"none":    nil,
		"managed": {Spec: simplyblockv1alpha2.ControlPlaneSpec{Source: simplyblockv1alpha2.ControlPlaneSource{Managed: &simplyblockv1alpha2.ManagedControlPlane{}}}},
	} {
		t.Run(name, func(t *testing.T) {
			got := inheritTLS(simplyblockv1alpha2.DriverTLS{}, cp)
			if *got.EnableTLS || *got.EnableMutualTLS {
				t.Errorf("got enableTLS=%v enableMutualTLS=%v, want both off", *got.EnableTLS, *got.EnableMutualTLS)
			}
		})
	}
}

// A driver that states no TLS, beside a ControlPlane with the default mutual
// TLS, is deployed with mutual TLS: the client certificates are issued.
func TestReconcileDeploysTheControlPlanesTLS(t *testing.T) {
	scheme := reconcilerScheme(t)
	d := testDriver("simplyblock")
	cp := localCP(simplyblockv1alpha2.ControlPlaneTLS{})
	cp.Name, cp.Namespace = controlplane.SingletonName, d.Namespace
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d, cp).WithStatusSubresource(d).Build()
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme, Snapshots: fixedSnapshotAPI{served: true}}

	if _, err := r.Reconcile(t.Context(), requestFor(d)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	key := client.ObjectKey{Namespace: d.Namespace, Name: "simplyblock-csi-controller-client"}
	if err := c.Get(t.Context(), key, cert); err != nil {
		t.Errorf("the controller plugin's client certificate was not issued: %v", err)
	}
	var stored simplyblockv1alpha2.SimplyblockDriver
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(d), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.TLS.EnableTLS != nil {
		t.Error("the inherited TLS was written into the stored spec, freezing it")
	}
}

// Regression: adoption compares the running plugin with the effective TLS, the
// one inherited from the ControlPlane, not the stored spec that states none.
func TestAdoptionComparesTheInheritedTLS(t *testing.T) {
	for name, tc := range map[string]struct {
		running     string
		wantRefused bool
	}{
		"running mutual TLS matches the inherited mutual TLS": {running: "authenticated"},
		"running plaintext differs from the inherited TLS":    {running: "", wantRefused: true},
	} {
		t.Run(name, func(t *testing.T) {
			scheme := reconcilerScheme(t)
			d := testDriver("simplyblock")
			cp := localCP(simplyblockv1alpha2.ControlPlaneTLS{})
			cp.Name, cp.Namespace = controlplane.SingletonName, d.Namespace
			node := chartInstalledNodeDaemonSet(d, DefaultDriverName)
			if tc.running != "" {
				node.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "SB_TLS_CONNECT", Value: tc.running}}
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d, cp, node).WithStatusSubresource(d).Build()
			r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme, Snapshots: fixedSnapshotAPI{served: true}}

			if _, err := r.Reconcile(t.Context(), requestFor(d)); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			var got simplyblockv1alpha2.SimplyblockDriver
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(d), &got); err != nil {
				t.Fatal(err)
			}
			refused := strings.Contains(got.Status.Message, "SB_TLS_CONNECT")
			if refused != tc.wantRefused {
				t.Errorf("refused = %v, want %v (status: %q)", refused, tc.wantRefused, got.Status.Message)
			}
		})
	}
}
