// Where the rest of the operator reads the control plane's address from.
//
// design-controlplane.md §3.3 makes status.endpoint the one answer to where the
// control plane is, so that naming a remote one is a field rather than the
// SIMPLYBLOCK_WEBAPI_BASE_URL environment variable, and so that a change to it
// reaches every reader without a Deployment rollout. This is what the readers
// call.
//
// It resolves rather than injects, because the endpoint is not known when the
// manager builds its controllers: the ControlPlane has not been reconciled then,
// and a remote one may not have been created at all. Every caller therefore
// asks per request and gets the current answer.
//
// An empty string means the object says nothing yet, and the caller keeps
// whatever default it already had. That is what makes adopting this additive: a
// deployment whose ControlPlane has not published an endpoint behaves exactly as
// it did before.

package controlplane

import (
	"context"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// endpointCacheTTL is how long a resolved endpoint is reused.
//
// The read is served from the manager's cache, so it is cheap, but it happens on
// every control-plane call the operator makes and those are frequent. A second
// is short enough that an endpoint change reaches every caller within one, and
// long enough that a burst of calls resolves once.
const endpointCacheTTL = time.Second

// EndpointResolver answers where the control plane is. An empty string means the
// ControlPlane has published no endpoint, and the caller keeps its own default.
type EndpointResolver func(ctx context.Context) string

// StretchResolver answers which site of a stretched control plane this
// operator's control plane is, nil when it is an ordinary one or cannot be
// read. The StorageCluster controller asks it when creating or adopting a
// backend cluster.
type StretchResolver func(ctx context.Context) *simplyblockv1alpha2.StretchedControlPlaneSpec

// NewStretchResolver returns a resolver reading the ControlPlane singleton in
// the operator's namespace. Like NewEndpointResolver it never errs: an
// unreadable singleton answers nil, which a caller treats as "not stretched
// as far as is known" and retries on its next pass.
func NewStretchResolver(reader client.Reader, namespace string) StretchResolver {
	return func(ctx context.Context) *simplyblockv1alpha2.StretchedControlPlaneSpec {
		var cp simplyblockv1alpha2.ControlPlane
		key := client.ObjectKey{Namespace: namespace, Name: SingletonName}
		if err := reader.Get(ctx, key, &cp); err != nil {
			return nil
		}
		return stretchOf(&cp).DeepCopy()
	}
}

// NewEndpointResolver returns a resolver reading the ControlPlane singleton in
// the operator's namespace.
//
// It never returns an error. A control plane that cannot be read is one whose
// endpoint is not known, which is the same situation as one that has not
// published it, and both mean the caller keeps its default. Every
// control-plane call in the operator resolves through this, so an unreadable
// singleton is a transient miss rather than a failed call.
func NewEndpointResolver(reader client.Reader, namespace string) EndpointResolver {
	var (
		mu       sync.Mutex
		cached   string
		cachedAt time.Time
	)

	return func(ctx context.Context) string {
		mu.Lock()
		defer mu.Unlock()

		if !cachedAt.IsZero() && time.Since(cachedAt) < endpointCacheTTL {
			return cached
		}

		var cp simplyblockv1alpha2.ControlPlane
		key := client.ObjectKey{Namespace: namespace, Name: SingletonName}
		if err := reader.Get(ctx, key, &cp); err != nil {
			// Not cached: an unreadable singleton is a transient state, and
			// caching the empty answer would hold every caller on its default
			// for the rest of the interval.
			return ""
		}

		cached, cachedAt = cp.Status.Endpoint, time.Now()
		return cached
	}
}
