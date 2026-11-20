// The TLS an installation uses, resolved from the ControlPlane.
//
// No ControlPlane is plaintext: an operator installed before its ControlPlane
// has nothing to be secure about yet.

package controlplane

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestTLSSettingsFollowTheControlPlane(t *testing.T) {
	withTLS := func(tls simplyblockv1alpha2.ControlPlaneTLS) *simplyblockv1alpha2.ControlPlane {
		cp := localControlPlane()
		cp.Spec.Source.Local.TLS = tls
		return cp
	}
	managed := func(endpoint string) *simplyblockv1alpha2.ControlPlane {
		cp := managedControlPlane(endpoint)
		cp.Status.Endpoint = endpoint
		return cp
	}
	ignored := localControlPlane()
	ignored.Name = "an-ignored-control-plane"

	for name, tc := range map[string]struct {
		cp   *simplyblockv1alpha2.ControlPlane
		want TLSSettings
	}{
		"local default is mutual TLS": {
			localControlPlane(), TLSSettings{Enabled: true, Mutual: true, Provider: "cert-manager"},
		},
		"disabling TLS disables mutual TLS": {
			withTLS(simplyblockv1alpha2.ControlPlaneTLS{EnableTLS: ptr.To(false), EnableMutualTLS: ptr.To(true)}),
			TLSSettings{},
		},
		"TLS without client certificates": {
			withTLS(simplyblockv1alpha2.ControlPlaneTLS{EnableTLS: ptr.To(true), EnableMutualTLS: ptr.To(false)}),
			TLSSettings{Enabled: true, Provider: "cert-manager"},
		},
		"OpenShift is spelled as the API spells it": {
			withTLS(simplyblockv1alpha2.ControlPlaneTLS{Provider: simplyblockv1alpha2.ControlPlaneTLSOpenShift}),
			TLSSettings{Enabled: true, Mutual: true, Provider: "OpenShift"},
		},
		"managed https endpoint":       {managed("https://sb.example.com:5000"), TLSSettings{Enabled: true}},
		"managed http endpoint":        {managed("http://sb.example.com:5000"), TLSSettings{}},
		"only the singleton answers":   {ignored, TLSSettings{}},
		"no ControlPlane is plaintext": {nil, TLSSettings{}},
	} {
		t.Run(name, func(t *testing.T) {
			var objects []client.Object
			if tc.cp != nil {
				objects = append(objects, tc.cp)
			}
			got := NewTLSResolver(newClient(t, objects...), testNamespace)(context.Background())
			if got != tc.want {
				t.Errorf("settings = %+v, want %+v", got, tc.want)
			}
		})
	}
}
