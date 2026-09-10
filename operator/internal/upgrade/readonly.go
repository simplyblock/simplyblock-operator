// The client a read-only stage runs against. §27 states that the plan mutates
// nothing, including through server-side dry-run writes, and §30.8 asserts that
// the preflight fails closed against a client that fails the test on any write.
// Both of those are this type: a check that writes is a bug in the check, and
// wrapping the client is what turns that bug into an error instead of into a
// changed cluster.

package upgrade

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrReadOnly is what every write against a [ReadOnlyClient] returns.
var ErrReadOnly = fmt.Errorf("this stage is read-only and may not write to the cluster")

// ReadOnlyClient passes reads through and refuses every write, including the
// status writer's and including a write carrying a dry-run option. A dry-run
// write is still a request the API server admits and every mutating webhook
// sees, so it is refused with the rest.
type ReadOnlyClient struct {
	client.Client
}

// NewReadOnlyClient wraps a client so that nothing it is handed to can write.
func NewReadOnlyClient(c client.Client) client.Client {
	return ReadOnlyClient{Client: c}
}

func (r ReadOnlyClient) Create(context.Context, client.Object, ...client.CreateOption) error {
	return fmt.Errorf("create: %w", ErrReadOnly)
}

func (r ReadOnlyClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return fmt.Errorf("delete: %w", ErrReadOnly)
}

func (r ReadOnlyClient) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return fmt.Errorf("update: %w", ErrReadOnly)
}

func (r ReadOnlyClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return fmt.Errorf("patch: %w", ErrReadOnly)
}

func (r ReadOnlyClient) Apply(context.Context, runtime.ApplyConfiguration, ...client.ApplyOption) error {
	return fmt.Errorf("apply: %w", ErrReadOnly)
}

func (r ReadOnlyClient) DeleteAllOf(context.Context, client.Object, ...client.DeleteAllOfOption) error {
	return fmt.Errorf("deleteAllOf: %w", ErrReadOnly)
}

// Status is the writer half only, so there is nothing here to pass through.
func (r ReadOnlyClient) Status() client.SubResourceWriter {
	return readOnlySubResource{}
}

// SubResource wraps the real subresource client rather than replacing it,
// because a subresource has a read as well as writes. Reading a status is a
// read like any other, and refusing it would fail a check that looks at one
// with a message saying the stage may not write.
func (r ReadOnlyClient) SubResource(subResource string) client.SubResourceClient {
	return readOnlySubResource{inner: r.Client.SubResource(subResource)}
}

// readOnlySubResource passes the subresource read through and refuses the
// status and scale writes a check has no reason to perform.
type readOnlySubResource struct {
	// inner is nil for the one built by Status, which is a writer and has no
	// read to delegate.
	inner client.SubResourceClient
}

func (r readOnlySubResource) Get(
	ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceGetOption,
) error {
	if r.inner == nil {
		return fmt.Errorf("subresource get: %w", ErrReadOnly)
	}
	return r.inner.Get(ctx, obj, subResource, opts...)
}

func (readOnlySubResource) Create(context.Context, client.Object, client.Object, ...client.SubResourceCreateOption) error {
	return fmt.Errorf("subresource create: %w", ErrReadOnly)
}

func (readOnlySubResource) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return fmt.Errorf("subresource update: %w", ErrReadOnly)
}

func (readOnlySubResource) Apply(context.Context, runtime.ApplyConfiguration, ...client.SubResourceApplyOption) error {
	return fmt.Errorf("subresource apply: %w", ErrReadOnly)
}

func (readOnlySubResource) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return fmt.Errorf("subresource patch: %w", ErrReadOnly)
}
