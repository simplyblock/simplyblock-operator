// What the rest of the operator authenticates a call to a managed control
// plane with.
//
// This mirrors NewEndpointResolver (endpoint.go, resolver.go): the same
// singleton, the same cache interval, and the same "unreadable is a transient
// miss, not an error" answer, because a caller with no credential falls back
// to whatever it already carries -- its own cluster-secret authentication, or
// none at all for a local control plane -- and that fallback is exactly what an
// admitting deployment looked like before this existed.

package controlplane

import (
	"context"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// CredentialResolver answers what a call to the control plane authenticates
// with, per call. The second return is false when the ControlPlane names no
// managed credential -- the caller then keeps whatever it already had.
type CredentialResolver func(ctx context.Context) (token string, ok bool)

// NewCredentialResolver returns a resolver reading the ControlPlane
// singleton's spec.source.managed.credentialsSecretRef, in the operator's
// namespace.
func NewCredentialResolver(reader client.Reader, namespace string) CredentialResolver {
	var (
		mu       sync.Mutex
		cached   string
		cachedOK bool
		cachedAt time.Time
	)

	return func(ctx context.Context) (string, bool) {
		mu.Lock()
		defer mu.Unlock()

		if !cachedAt.IsZero() && time.Since(cachedAt) < endpointCacheTTL {
			return cached, cachedOK
		}

		var cp simplyblockv1alpha2.ControlPlane
		key := client.ObjectKey{Namespace: namespace, Name: SingletonName}
		if err := reader.Get(ctx, key, &cp); err != nil {
			// Not cached, for the same reason NewEndpointResolver does not cache
			// this case: an unreadable singleton is transient, and caching the
			// negative answer would hold every caller on no credential for the
			// rest of the interval.
			return "", false
		}

		managed := cp.Spec.Source.Managed
		if managed == nil || managed.CredentialsSecretRef == nil || managed.CredentialsSecretRef.Name == "" {
			cached, cachedOK, cachedAt = "", false, time.Now()
			return cached, cachedOK
		}

		var secret corev1.Secret
		secretKey := client.ObjectKey{Namespace: namespace, Name: managed.CredentialsSecretRef.Name}
		if err := reader.Get(ctx, secretKey, &secret); err != nil {
			return "", false
		}

		for _, k := range credentialKeys {
			if value := strings.TrimSpace(string(secret.Data[k])); value != "" {
				cached, cachedOK, cachedAt = value, true, time.Now()
				return cached, cachedOK
			}
		}
		cached, cachedOK, cachedAt = "", false, time.Now()
		return cached, cachedOK
	}
}
