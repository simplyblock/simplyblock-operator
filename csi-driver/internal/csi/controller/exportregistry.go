// The NFSExport registry backed by a dynamic client.
//
// The driver holds a typed kubernetes.Interface, which cannot touch a custom
// resource, and generating a typed client for one kind the driver only creates
// and reads would be a build-time dependency on the operator's module for very
// little. A dynamic client is the smaller thing: four fields in and a phase out.
//
// What this deliberately does not do is watch. The controller creates the record
// and then reads it back on each retried CreateVolume, because the external
// provisioner is already the retry loop and a watch here would be a second one.

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

// identityWriteAttempts bounds the conflict retry. The competing writer is the
// operator recording the host it picked, which it does once, so a couple of
// attempts is enough and CreateVolume is itself retried by the provisioner.
const identityWriteAttempts = 3

// nfsExportGVR is the resource the records live in. It is v1alpha2 because the
// kind is new: nothing ever shipped a v1alpha1 spelling of it.
var nfsExportGVR = schema.GroupVersionResource{
	Group:    "storage.simplyblock.io",
	Version:  "v1alpha2",
	Resource: "nfsexports",
}

// dynamicExportRegistry creates and reads NFSExport records.
type dynamicExportRegistry struct {
	client dynamic.Interface
}

// NewExportRegistry returns a registry over a dynamic client, or nil when there
// is none. A nil registry is not an error here: the driver runs outside a
// cluster in tests, and the pNFS path refuses the claim with a clear message
// rather than the driver failing to start.
func NewExportRegistry(client dynamic.Interface) ExportRegistry {
	if client == nil {
		return nil
	}
	return &dynamicExportRegistry{client: client}
}

// EnsureExport creates the record if it is absent and returns it either way.
//
// Create-or-fetch rather than create, because the external provisioner retries
// CreateVolume and a retry must not leave a second export behind. The name is
// derived from the volume's identity, so the second attempt addresses the same
// object the first one made.
func (r *dynamicExportRegistry) EnsureExport(
	ctx context.Context,
	name string,
	spec ExportSpec,
) (ExportRecord, error) {
	api := r.client.Resource(nfsExportGVR).Namespace(spec.Namespace)

	existing, err := api.Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return r.ensureIdentity(ctx, api, existing, spec)
	case !apierrors.IsNotFound(err):
		return ExportRecord{}, fmt.Errorf("reading export %s: %w", name, err)
	}

	created, err := api.Create(ctx, desiredExport(name, spec), metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another attempt won the race. Read what it made rather than
			// failing: both attempts are provisioning the same volume.
			existing, getErr := api.Get(ctx, name, metav1.GetOptions{})
			if getErr != nil {
				return ExportRecord{}, fmt.Errorf("reading export %s after a create race: %w", name, getErr)
			}
			return r.ensureIdentity(ctx, api, existing, spec)
		}
		return ExportRecord{}, fmt.Errorf("creating export %s: %w", name, err)
	}

	// The status carrying the backing volume's identity is written here rather
	// than left for the operator, because the CSI controller is the only thing
	// that knows it: it just created the volume. The operator reads it back to
	// tell the host which device to assemble on.
	return r.ensureIdentity(ctx, api, created, spec)
}

// ensureIdentity puts the backing volume on the record's status if it is not
// there already, and reports the record either way.
//
// It runs on every path rather than only after a create, which is the whole
// point. The operator holds in Assembling until status.lvolID arrives, and it
// writes the same status object to record the host it picked, so the two writes
// race. Losing that race returns a conflict, and if the identity were only
// written by the create branch, the retried CreateVolume would take the fetch
// branch, skip the write, and return Aborted forever against an operator
// waiting for a field that would never be set. That is a deadlock rather than a
// delay, and it is not specific to any access mode: it is whichever claim loses
// the race.
//
// The write is skipped when the identity is already there, so a retry does not
// churn the object, and it never touches any other status field.
func (r *dynamicExportRegistry) ensureIdentity(
	ctx context.Context,
	api dynamic.ResourceInterface,
	object *unstructured.Unstructured,
	spec ExportSpec,
) (ExportRecord, error) {
	if spec.LVolID == "" {
		return recordFrom(object), nil
	}
	if current, _, _ := unstructured.NestedString(object.Object, "status", "lvolID"); current != "" {
		return recordFrom(object), nil
	}

	// Re-read before each attempt: the object in hand may already be stale,
	// which is exactly the case this exists for.
	var last error
	for attempt := 0; attempt < identityWriteAttempts; attempt++ {
		fresh, err := api.Get(ctx, object.GetName(), metav1.GetOptions{})
		if err != nil {
			return ExportRecord{}, fmt.Errorf("reading export %s: %w", object.GetName(), err)
		}
		if current, _, _ := unstructured.NestedString(fresh.Object, "status", "lvolID"); current != "" {
			// Another attempt wrote it.
			return recordFrom(fresh), nil
		}
		if err := unstructured.SetNestedField(fresh.Object, spec.LVolID, "status", "lvolID"); err != nil {
			return ExportRecord{}, fmt.Errorf("building the status of export %s: %w", object.GetName(), err)
		}
		if spec.NGUID != "" {
			if err := unstructured.SetNestedField(fresh.Object, spec.NGUID, "status", "nguid"); err != nil {
				return ExportRecord{}, fmt.Errorf("building the status of export %s: %w", object.GetName(), err)
			}
		}
		// Only set the phase when the operator has not set one, so this never
		// moves a record backward out of a phase the operator is driving.
		if phase, _, _ := unstructured.NestedString(fresh.Object, "status", "phase"); phase == "" {
			if err := unstructured.SetNestedField(fresh.Object, "Pending", "status", "phase"); err != nil {
				return ExportRecord{}, fmt.Errorf("building the status of export %s: %w", object.GetName(), err)
			}
		}
		updated, err := api.UpdateStatus(ctx, fresh, metav1.UpdateOptions{})
		if err == nil {
			return recordFrom(updated), nil
		}
		if !apierrors.IsConflict(err) {
			return ExportRecord{}, fmt.Errorf(
				"recording the backing volume of export %s: %w", object.GetName(), err)
		}
		last = err
	}
	return ExportRecord{}, fmt.Errorf(
		"recording the backing volume of export %s: %w", object.GetName(), last)
}

// desiredExport is the object to create. Every spec field is immutable on the
// record, so all of them are set here: the record cannot be filled in later.
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
			"fsid":       spec.FSID,
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
		FSID:           get("spec", "fsid"),
		NGUID:          get("status", "nguid"),
		Message:        get("status", "message"),
	}
}

// DeleteExport removes the record and reports whether it is gone.
//
// The record is found by listing every namespace rather than fetched by key,
// because DeleteVolume is handed a volume handle and nothing else: not the
// claim the volume belonged to, and not the namespace it was in. The name is a
// digest of the volume's identity, so at most one object in the cluster carries
// it and the list is unambiguous.
//
// Not-gone is the normal answer on the first call. The operator holds a
// finalizer while it unmounts, unpublishes, and detaches on the host, so the
// object outlives the delete by however long that takes, and the backing volume
// must not be deleted until it has finished.
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
