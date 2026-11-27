// Fills the StorageProfile KubeVirt's CDI keeps for each StorageClass of a
// simplyblock driver.
//
// CDI ships a table of the provisioners it knows, and simplyblock is not in
// it, so every profile of ours starts empty. An empty profile names no access
// mode, which makes CDI refuse any DataVolume that does not spell out its own,
// and a VM whose disk was not claimed ReadWriteMany cannot live-migrate. This
// writes what each class supports (atlas-lib's kube.ClaimShapesFor), the clone
// strategy, and the format golden images are kept in, into every field the
// profile leaves empty. A field anyone has set is theirs and is never
// rewritten.
//
// The profile is read and written unstructured: no other package here needs
// CDI's types, and the operator has to work against whichever CDI the cluster
// runs.

package virtualization

import (
	"context"
	"fmt"
	"time"

	"github.com/simplyblock/atlas/kube"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
)

// StorageProfileGVK is CDI's StorageProfile.
var StorageProfileGVK = schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "StorageProfile"}

// The values CDI accepts for the two strategy fields this writes.
const (
	// cloneCSI asks the driver for the clone, which is a copy-on-write lvol.
	cloneCSI = "csi-clone"
	// cloneCopy has CDI copy the data through a pod, for a class the driver
	// cannot clone into.
	cloneCopy = "copy"
	// sourceSnapshot keeps golden images as VolumeSnapshots, which a VM's disk
	// is then cloned from without a copy.
	sourceSnapshot = "snapshot"
	// sourcePVC keeps them as volumes.
	sourcePVC = "pvc"
)

// StorageProfileFilled is the event reason for a profile this wrote.
const StorageProfileFilled = "StorageProfileFilled"

// conflictRetry is how soon a fill that lost a race with another writer reads
// the profile again.
const conflictRetry = time.Second

// StorageProfileReconciler fills the empty fields of simplyblock's
// StorageProfiles.
type StorageProfileReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=storageprofiles,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=simplyblockdrivers,verbs=get;list;watch

// Reconcile fills one StorageProfile. CDI names a profile after its class and
// reports the class's provisioner in status, so a profile becomes ours the
// moment CDI has looked at the class.
func (r *StorageProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	profile := &unstructured.Unstructured{}
	profile.SetGroupVersionKind(StorageProfileGVK)
	if err := r.Get(ctx, req.NamespacedName, profile); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	provisioner, _, _ := unstructured.NestedString(profile.Object, "status", "provisioner")
	ours, err := r.isSimplyblockDriver(ctx, provisioner)
	if err != nil || !ours {
		return ctrl.Result{}, err
	}

	className, _, _ := unstructured.NestedString(profile.Object, "status", "storageClass")
	if className == "" {
		className = profile.GetName()
	}
	class := &storagev1.StorageClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: className}, class); err != nil {
		// CDI deletes the profile with the class, and that delete is the next
		// event.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	before := profile.DeepCopy()
	filled, err := fillEmpty(profile, class.Parameters)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(filled) == 0 {
		return ctrl.Result{}, nil
	}
	// The patch carries the resourceVersion it was computed from: a field set
	// after the read makes it conflict rather than be overwritten, and the next
	// pass reads the profile again.
	if err := r.Patch(ctx, profile, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: conflictRetry}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fill StorageProfile %s: %w", profile.GetName(), err)
	}
	log.Info("filled the StorageProfile", "storageProfile", profile.GetName(), "fields", filled)
	if r.Recorder != nil {
		r.Recorder.Eventf(profile, nil, corev1.EventTypeNormal, StorageProfileFilled, StorageProfileFilled,
			"Filled %v for StorageClass %s", filled, className)
	}
	return ctrl.Result{}, nil
}

// isSimplyblockDriver reports whether provisioner is a name a simplyblock CSI
// driver registers: the default one the chart uses, or the one a
// SimplyblockDriver names.
func (r *StorageProfileReconciler) isSimplyblockDriver(ctx context.Context, provisioner string) (bool, error) {
	if provisioner == "" {
		return false, nil
	}
	if provisioner == driver.DefaultDriverName {
		return true, nil
	}
	drivers := &simplyblockv1alpha2.SimplyblockDriverList{}
	if err := r.List(ctx, drivers); err != nil {
		return false, fmt.Errorf("list SimplyblockDrivers: %w", err)
	}
	for i := range drivers.Items {
		if driver.DriverName(&drivers.Items[i]) == provisioner {
			return true, nil
		}
	}
	return false, nil
}

// fillEmpty writes the fields of the profile's spec that are empty and returns
// their names.
func fillEmpty(profile *unstructured.Unstructured, params map[string]string) ([]string, error) {
	var filled []string
	set := func(field string, value any) error {
		if current, found, _ := unstructured.NestedFieldNoCopy(profile.Object, "spec", field); found && !isEmpty(current) {
			return nil
		}
		if err := unstructured.SetNestedField(profile.Object, value, "spec", field); err != nil {
			return fmt.Errorf("set spec.%s: %w", field, err)
		}
		filled = append(filled, field)
		return nil
	}

	clone, source := cloneCSI, sourceSnapshot
	if !kube.CanBeCloneTarget(params) {
		clone, source = cloneCopy, sourcePVC
	}
	for _, f := range []struct {
		field string
		value any
	}{
		{"claimPropertySets", claimPropertySets(params)},
		{"cloneStrategy", clone},
		{"dataImportCronSourceFormat", source},
	} {
		if err := set(f.field, f.value); err != nil {
			return nil, err
		}
	}
	return filled, nil
}

// claimPropertySets is kube.ClaimShapesFor in CDI's spelling: one set per
// shape, in the same order, since CDI takes the first for a claim that names
// no mode.
func claimPropertySets(params map[string]string) []any {
	shapes := kube.ClaimShapesFor(params)
	sets := make([]any, 0, len(shapes))
	for _, s := range shapes {
		sets = append(sets, map[string]any{
			"accessModes": []any{string(s.AccessMode)},
			"volumeMode":  string(s.VolumeMode),
		})
	}
	return sets
}

// isEmpty reports whether a spec field holds nothing: an empty string, list,
// or map, or null.
func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// SetupWithManager registers the reconciler. It watches only the profiles: CDI
// creates one for every class and writes the provisioner into its status,
// which is the event that brings a new class here.
func (r *StorageProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	profile := &unstructured.Unstructured{}
	profile.SetGroupVersionKind(StorageProfileGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(profile).
		Named("storageprofile").
		Complete(r)
}
