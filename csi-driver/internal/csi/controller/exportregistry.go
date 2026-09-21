// The NFSExport registry, over a dynamic client.
//
// Dynamic rather than typed: generating a typed client for one kind the driver
// only creates and reads would be a build dependency on the operator's module
// for four fields and a phase.
//
// It deliberately does not watch. The record is read back on each retried
// CreateVolume, because the external provisioner is already the retry loop.

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// nfsExportGVR is v1alpha2 because the kind is new.
var nfsExportGVR = schema.GroupVersionResource{
	Group:    "storage.simplyblock.io",
	Version:  "v1alpha2",
	Resource: "nfsexports",
}

// dynamicExportRegistry creates and reads NFSExport records.
type dynamicExportRegistry struct {
	client dynamic.Interface
}

// NewExportRegistry returns nil when there is no client. Not an error: the
// driver runs outside a cluster in tests, and the pNFS path refuses the claim
// rather than the driver failing to start.
func NewExportRegistry(client dynamic.Interface) ExportRegistry {
	if client == nil {
		return nil
	}
	return &dynamicExportRegistry{client: client}
}

// EnsureExport creates the record if it is absent and returns it either way.
// Create-or-fetch, because the provisioner retries and the name is derived, so
// the second attempt addresses the object the first one made.
//
// It writes the spec and nothing else. Status belongs to the operator, and the
// one field this used to put there raced the operator's own writes.
func (r *dynamicExportRegistry) EnsureExport(
	ctx context.Context,
	name string,
	spec ExportSpec,
) (ExportRecord, error) {
	api := r.client.Resource(nfsExportGVR).Namespace(spec.Namespace)

	existing, err := api.Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return recordFrom(existing), nil
	case !apierrors.IsNotFound(err):
		return ExportRecord{}, fmt.Errorf("reading export %s: %w", name, err)
	}

	created, err := api.Create(ctx, desiredExport(name, spec), metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another attempt won the race; both provision the same volume.
			existing, getErr := api.Get(ctx, name, metav1.GetOptions{})
			if getErr != nil {
				return ExportRecord{}, fmt.Errorf("reading export %s after a create race: %w", name, getErr)
			}
			return recordFrom(existing), nil
		}
		return ExportRecord{}, fmt.Errorf("creating export %s: %w", name, err)
	}

	return recordFrom(created), nil
}

// desiredExport is the object to create. Every spec field is immutable, so
// the record cannot be filled in later.
func desiredExport(name string, spec ExportSpec) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": nfsExportGVR.Group + "/" + nfsExportGVR.Version,
		"kind":       "NFSExport",
		"metadata": map[string]any{
			"name":      name,
			"namespace": spec.Namespace,
		},
		"spec": map[string]any{
			"volumeRef":  spec.VolumeRef,
			"exportPath": spec.ExportPath,
		},
	}}
}

// recordFrom reads back the fields provisioning waits on.
func recordFrom(object *unstructured.Unstructured) ExportRecord {
	get := func(keys ...string) string {
		value, _, _ := unstructured.NestedString(object.Object, keys...)
		return value
	}
	return ExportRecord{
		Phase:          get("status", "phase"),
		ServiceAddress: get("status", "mdsNodeIP"),
		ExportPath:     get("spec", "exportPath"),
		Message:        get("status", "message"),
	}
}

// DeleteExport removes the record and reports whether it is gone.
//
// Listed across namespaces rather than fetched by key, because DeleteVolume is
// handed a handle and nothing else. The name carries the volume's UUID, so at
// most one object in the cluster has it.
//
// Not-gone is the normal first answer: the operator holds a finalizer while it
// tears the host down, and the backing volume must wait for that.
func (r *dynamicExportRegistry) DeleteExport(ctx context.Context, name string) (bool, error) {
	all := r.client.Resource(nfsExportGVR).Namespace(metav1.NamespaceAll)
	listed, err := all.List(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
	})
	if err != nil {
		return false, fmt.Errorf("finding export %s: %w", name, err)
	}
	if len(listed.Items) == 0 {
		return true, nil
	}

	found := listed.Items[0]
	api := r.client.Resource(nfsExportGVR).Namespace(found.GetNamespace())
	if err := api.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("deleting export %s: %w", name, err)
	}
	// Deleted is not gone. Say so, and let the caller come back.
	return false, nil
}

// SetExportSize records a volume's new capacity on the export serving it, and
// reports whether there was one.
//
// The write is what matters, not the value: it bumps the record's generation,
// and that is what tells the operator to re-assemble and grow the filesystem on
// the host. Skipped when the size already matches, so a retried expand does not
// churn the object.
//
// Listed across namespaces for DeleteExport's reason: the caller has a volume
// handle and nothing else, and the name carries the volume's own id.
func (r *dynamicExportRegistry) SetExportSize(
	ctx context.Context, name string, bytes int64,
) (bool, error) {
	listed, err := r.client.Resource(nfsExportGVR).Namespace(metav1.NamespaceAll).
		List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
	if err != nil {
		return false, fmt.Errorf("finding export %s: %w", name, err)
	}
	if len(listed.Items) == 0 {
		return false, nil
	}

	found := listed.Items[0]
	if current, _, _ := unstructured.NestedInt64(found.Object, "spec", "sizeBytes"); current == bytes {
		return true, nil
	}
	if err := unstructured.SetNestedField(found.Object, bytes, "spec", "sizeBytes"); err != nil {
		return false, fmt.Errorf("building the spec of export %s: %w", name, err)
	}
	api := r.client.Resource(nfsExportGVR).Namespace(found.GetNamespace())
	if _, err := api.Update(ctx, &found, metav1.UpdateOptions{}); err != nil {
		return false, fmt.Errorf("recording the size of export %s: %w", name, err)
	}
	return true, nil
}
