// The TLS an installation uses, as the ControlPlane states it.
//
// The ControlPlane is the one place TLS is stated. The operator process carries
// no TLS setting of its own, so everything that has to agree with the control
// plane (the storage-node workloads, and the clients that reach the management
// API) asks this per use and a change reaches all of them without a rollout.
//

package controlplane

import (
	"context"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// TLSSettings is how the installation's control plane is reached and how the
// storage plane is expected to be reached.
type TLSSettings struct {
	// Enabled is whether the management API and the storage-node API are served
	// over TLS.
	Enabled bool

	// Mutual is whether callers present a client certificate. It is never true
	// without Enabled.
	Mutual bool

	// Provider is who issues the serving certificates, spelled as the API spells
	// it: cert-manager or OpenShift. Empty where TLS is off.
	Provider string
}

// TLSResolver answers what TLS the installation uses. The zero value is
// plaintext, which is what it returns where there is no ControlPlane to ask.
type TLSResolver func(ctx context.Context) TLSSettings

// Settings asks the resolver, and answers plaintext where there is none.
func (r TLSResolver) Settings(ctx context.Context) TLSSettings {
	if r == nil {
		return TLSSettings{}
	}
	return r(ctx)
}

// NewTLSResolver returns a resolver reading the ControlPlane singleton in the
// operator's namespace. A ControlPlane that cannot be read is plaintext, the
// same as one that does not exist yet; the reader is the manager's cache, so
// asking per use costs no API call.
func NewTLSResolver(reader client.Reader, namespace string) TLSResolver {
	return func(ctx context.Context) TLSSettings {
		var cp simplyblockv1alpha2.ControlPlane
		key := client.ObjectKey{Namespace: namespace, Name: SingletonName}
		if err := reader.Get(ctx, key, &cp); err != nil {
			return TLSSettings{}
		}
		return tlsSettingsOf(&cp)
	}
}

// tlsSettingsOf reads the settings off one object.
func tlsSettingsOf(cp *simplyblockv1alpha2.ControlPlane) TLSSettings {
	if local := cp.Spec.Source.Local; local != nil {
		if !local.ServesTLS() {
			return TLSSettings{}
		}
		return TLSSettings{
			Enabled:  true,
			Mutual:   local.RequiresClientCertificate(),
			Provider: string(local.TLSProvider()),
		}
	}

	// A control plane elsewhere has no TLS block, because it is not this
	// operator's to configure. Its endpoint says whether it is reached over TLS,
	// and no client certificate is supplied for it.
	endpoint := cp.Status.Endpoint
	if endpoint == "" && cp.Spec.Source.Managed != nil {
		endpoint = cp.Spec.Source.Managed.Endpoint
	}
	return TLSSettings{Enabled: strings.HasPrefix(endpoint, "https://")}
}
