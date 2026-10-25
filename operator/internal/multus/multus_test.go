// The Multus surface the operator shares: the networks annotation key, the
// NetworkAttachmentDefinition kind, and reading a definition by name with the
// three answers a caller has to tell apart.

package multus

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func definitionScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(DefinitionGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(DefinitionGVK.GroupVersion().WithKind(DefinitionGVK.Kind+"List"), &unstructured.UnstructuredList{})
	return scheme
}

func TestGetDefinitionFound(t *testing.T) {
	nad := NewDefinition()
	nad.SetNamespace("simplyblock")
	nad.SetName("storage-data")
	c := fake.NewClientBuilder().WithScheme(definitionScheme()).WithObjects(nad).Build()

	got, err := GetDefinition(context.Background(), c, client.ObjectKey{Namespace: "simplyblock", Name: "storage-data"})
	if err != nil {
		t.Fatalf("GetDefinition: %v", err)
	}
	if got.GetName() != "storage-data" {
		t.Fatalf("name = %q", got.GetName())
	}
}

func TestGetDefinitionNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(definitionScheme()).Build()

	_, err := GetDefinition(context.Background(), c, client.ObjectKey{Namespace: "simplyblock", Name: "absent"})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
	if KindNotServed(err) {
		t.Fatal("a missing definition was reported as a kind that is not served")
	}
}

func TestGetDefinitionKindNotServed(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(definitionScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return &meta.NoKindMatchError{GroupKind: DefinitionGVK.GroupKind(), SearchedVersions: []string{"v1"}}
			},
		}).Build()

	_, err := GetDefinition(context.Background(), c, client.ObjectKey{Namespace: "simplyblock", Name: "storage-data"})
	if !KindNotServed(err) {
		t.Fatalf("err = %v, want kind not served", err)
	}
	if apierrors.IsNotFound(err) {
		t.Fatal("a kind that is not served was reported as a missing definition")
	}
}
