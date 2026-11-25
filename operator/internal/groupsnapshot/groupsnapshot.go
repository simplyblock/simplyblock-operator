// Package groupsnapshot reads the upstream VolumeGroupSnapshot kinds
// (groupsnapshot.storage.k8s.io) at whichever version the cluster serves.
//
// The group has been served as v1beta1, v1beta2, and v1, and a cluster serves
// whichever its snapshot controller ships: OpenShift 5 serves v1 only, a
// cluster on the external-snapshotter 8.2 CRDs serves v1beta1 only. A typed
// read pinned to one version fails on every cluster serving another, so the
// operator reads these objects unstructured, at the newest version the cluster
// serves, into the few fields it uses. Those fields have the same names and
// meaning in all three versions.
//
// The types here describe objects the operator reads, not kinds it defines, so
// controller-gen is told to generate nothing for them.
//
// +kubebuilder:skip
package groupsnapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Group is the API group of the upstream VolumeGroupSnapshot kinds.
const Group = "groupsnapshot.storage.k8s.io"

// The kinds this package reads.
const (
	KindVolumeGroupSnapshot        = "VolumeGroupSnapshot"
	KindVolumeGroupSnapshotClass   = "VolumeGroupSnapshotClass"
	KindVolumeGroupSnapshotContent = "VolumeGroupSnapshotContent"
)

// Versions are the versions of Group whose shape this package knows, newest
// first. A cluster serving several is read at the first of them it serves.
var Versions = []string{"v1", "v1beta2", "v1beta1"}

// VolumeGroupSnapshot is the part of a VolumeGroupSnapshot the operator reads.
type VolumeGroupSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeGroupSnapshotSpec    `json:"spec"`
	Status *VolumeGroupSnapshotStatus `json:"status,omitempty"`
}

// VolumeGroupSnapshotSpec is the part of the spec the operator reads.
type VolumeGroupSnapshotSpec struct {
	Source                       VolumeGroupSnapshotSource `json:"source"`
	VolumeGroupSnapshotClassName *string                   `json:"volumeGroupSnapshotClassName,omitempty"`
}

// VolumeGroupSnapshotSource selects the claims a group snapshot covers.
type VolumeGroupSnapshotSource struct {
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// VolumeGroupSnapshotStatus is the part of the status the operator reads.
type VolumeGroupSnapshotStatus struct {
	BoundVolumeGroupSnapshotContentName *string `json:"boundVolumeGroupSnapshotContentName,omitempty"`
	ReadyToUse                          *bool   `json:"readyToUse,omitempty"`
}

// VolumeGroupSnapshotClass is the part of a VolumeGroupSnapshotClass the
// operator reads.
type VolumeGroupSnapshotClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Driver string `json:"driver"`
}

// VolumeGroupSnapshotContent is the part of a VolumeGroupSnapshotContent the
// operator reads.
type VolumeGroupSnapshotContent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec VolumeGroupSnapshotContentSpec `json:"spec"`
}

// VolumeGroupSnapshotContentSpec is the part of the content's spec the
// operator reads.
type VolumeGroupSnapshotContentSpec struct {
	Source VolumeGroupSnapshotContentSource `json:"source"`
}

// VolumeGroupSnapshotContentSource names the volumes a group snapshot was
// taken of.
type VolumeGroupSnapshotContentSource struct {
	VolumeHandles []string `json:"volumeHandles,omitempty"`
}

// GetVolumeGroupSnapshot reads a VolumeGroupSnapshot. A cluster that serves no
// version of the group answers NotFound, as for an object that does not exist.
func GetVolumeGroupSnapshot(ctx context.Context, c client.Client, key client.ObjectKey) (*VolumeGroupSnapshot, error) {
	out := &VolumeGroupSnapshot{}
	return out, get(ctx, c, KindVolumeGroupSnapshot, key, out)
}

// GetVolumeGroupSnapshotClass reads a VolumeGroupSnapshotClass, answering as
// GetVolumeGroupSnapshot does.
func GetVolumeGroupSnapshotClass(ctx context.Context, c client.Client, name string) (*VolumeGroupSnapshotClass, error) {
	out := &VolumeGroupSnapshotClass{}
	return out, get(ctx, c, KindVolumeGroupSnapshotClass, client.ObjectKey{Name: name}, out)
}

// GetVolumeGroupSnapshotContent reads a VolumeGroupSnapshotContent, answering
// as GetVolumeGroupSnapshot does.
func GetVolumeGroupSnapshotContent(
	ctx context.Context, c client.Client, name string,
) (*VolumeGroupSnapshotContent, error) {
	out := &VolumeGroupSnapshotContent{}
	return out, get(ctx, c, KindVolumeGroupSnapshotContent, client.ObjectKey{Name: name}, out)
}

// DecodeVolumeGroupSnapshot decodes a VolumeGroupSnapshot an admission request
// carries, refusing a version whose shape this package does not know.
func DecodeVolumeGroupSnapshot(raw []byte) (*VolumeGroupSnapshot, error) {
	out := &VolumeGroupSnapshot{}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, err
	}
	gv, err := schema.ParseGroupVersion(out.APIVersion)
	if err != nil {
		return nil, err
	}
	if gv.Group != Group || !slices.Contains(Versions, gv.Version) {
		return nil, fmt.Errorf("%s is not a VolumeGroupSnapshot version this operator reads", out.APIVersion)
	}
	return out, nil
}

// get reads the object at the newest version the cluster serves. The read is
// unstructured, so it goes to the API server rather than starting an informer
// on a version that may not be served.
func get(ctx context.Context, c client.Client, kind string, key client.ObjectKey, out any) error {
	gk := schema.GroupKind{Group: Group, Kind: kind}
	mapping, err := c.RESTMapper().RESTMapping(gk, Versions...)
	if meta.IsNoMatchError(err) {
		return apierrors.NewNotFound(schema.GroupResource{Group: Group, Resource: kind}, key.Name)
	}
	if err != nil {
		return fmt.Errorf("resolve the served version of %s: %w", gk, err)
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(mapping.GroupVersionKind)
	if err := c.Get(ctx, key, u); err != nil {
		return err
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out)
}
