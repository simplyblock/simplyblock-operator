// The Multus surface the operator shares: the networks annotation key, the
// NetworkAttachmentDefinition kind, and reading a definition by name, shared by
// the driver and storage node reconcilers.

package multus

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NetworksAnnotation is Multus's: the networks a pod is attached to beside its
// default one.
const NetworksAnnotation = "k8s.v1.cni.cncf.io/networks"

// DefinitionGVK is Multus's NetworkAttachmentDefinition, read and written
// unstructured.
var DefinitionGVK = schema.GroupVersionKind{
	Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition",
}

// NewDefinition is an empty NetworkAttachmentDefinition with its kind set, the
// form a Get or a Create needs.
func NewDefinition() *unstructured.Unstructured {
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(DefinitionGVK)
	return nad
}

// GetDefinition reads the definition at key. The error is the API's not-found
// when it does not exist, and one for which KindNotServed is true when the
// cluster has no such kind, which is a cluster without Multus.
func GetDefinition(ctx context.Context, c client.Reader, key client.ObjectKey) (*unstructured.Unstructured, error) {
	nad := NewDefinition()
	if err := c.Get(ctx, key, nad); err != nil {
		return nil, err
	}
	return nad, nil
}

// KindNotServed reports whether err says the cluster serves no
// NetworkAttachmentDefinition kind.
func KindNotServed(err error) bool {
	return meta.IsNoMatchError(err)
}
