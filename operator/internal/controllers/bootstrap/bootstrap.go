// Package bootstrap applies the objects the Helm chart hands the operator in a
// ConfigMap: the ControlPlane, the SimplyblockDriver, and the initial discovery
// run.
//
// # Why the chart does not render them itself
//
// The chart installs the CRDs of these kinds from its crds/ directory and, in
// the same release, would render objects of them. Helm creates the CRDs and then
// maps every release object against the API server's discovery. On a fresh
// cluster (EKS in particular) discovery does not list the new kinds for a while,
// so the install fails with "no matches for kind ControlPlane ... ensure CRDs
// are installed first". A retry skips the CRDs, which exist by then, and maps
// against the same stale discovery, so it fails again until the cache catches up.
//
// A ConfigMap is a core kind that always maps. The chart renders the three
// objects into one (one key per object, each a YAML document) and this
// reconciler server-side applies them, retrying with a backoff while the kind
// is not yet served.
//
// # What it applies, and when
//
// A key is applied when its content differs from what this reconciler last
// applied for it, which it records in an annotation on the ConfigMap. A Helm
// upgrade that changes the rendered object changes the key and the object is
// applied again; an upgrade that leaves it alone does not touch the object. That
// is also why a ControlPlane deleted by hand is not brought back on the next
// operator restart: recreating one rebuilds an empty FoundationDB, and that has
// to be somebody's decision.
//
// Removing a key never deletes the object it created. A values change that drops
// the ControlPlane must not take the database behind every volume with it, so
// deleting one of these objects stays a deliberate `kubectl delete`.
//
// The one exception is the discovery run, which carries an owner reference to
// the ConfigMap when it lives in the same namespace, so `helm uninstall` (which
// deletes the ConfigMap) still removes it as it did when Helm owned it.
package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/yaml"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// ConfigMapName is the ConfigMap the chart renders the objects into, in the
	// operator's namespace.
	ConfigMapName = "simplyblock-bootstrap-objects"

	// FieldOwner is the server-side-apply field manager the objects are applied
	// with. It stands in for Helm, which owned these fields before.
	FieldOwner = "simplyblock-chart"

	// AppliedAnnotation records, per key, the digest of the content last applied.
	AppliedAnnotation = "storage.simplyblock.io/bootstrap-applied"

	// ReleaseIsInstallAnnotation is rendered by the chart: "true" when the
	// ConfigMap was rendered by `helm install`, "false" by `helm upgrade`.
	ReleaseIsInstallAnnotation = "storage.simplyblock.io/release-is-install"

	group = "storage.simplyblock.io"

	// The backoff while a kind is not yet served, or an apply is refused. Short at
	// the start, because the usual cause is discovery lagging a CRD by seconds.
	backoffBase = time.Second
	backoffMax  = 30 * time.Second
)

// allowedKinds are the only kinds this reconciler applies. Whoever can write a
// ConfigMap in the operator's namespace could otherwise have the operator apply
// anything its ClusterRole allows.
var allowedKinds = map[string]bool{
	"ControlPlane":      true,
	"OperatorOps":       true,
	"SimplyblockDriver": true,
}

// Reconciler applies the objects of the chart's bootstrap ConfigMap.
type Reconciler struct {
	Client    client.Client
	Recorder  events.EventRecorder
	Namespace string
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=controlplanes;simplyblockdrivers;operatorops,verbs=get;list;watch;create;patch

// Reconcile applies every key whose content changed since it was last applied.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var cm corev1.ConfigMap
	if err := r.Client.Get(ctx, req.NamespacedName, &cm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cm.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	applied := map[string]string{}
	_, recorded := cm.Annotations[AppliedAnnotation]
	if recorded {
		if err := json.Unmarshal([]byte(cm.Annotations[AppliedAnnotation]), &applied); err != nil {
			// A mangled record is treated as none: every key is applied again,
			// which server-side apply makes harmless for the objects that exist.
			logger.Info("ignoring an unreadable applied-record annotation", "error", err.Error())
			applied = map[string]string{}
		}
	}
	// The ConfigMap first appearing on an upgrade, with no record yet, is a
	// release moving from a chart that rendered these objects itself. Helm has
	// just deleted the discovery run it owned (the run carries no keep policy),
	// and creating it again would start a second discovery on a cluster that
	// already finished its first.
	migrating := !recorded && cm.Annotations[ReleaseIsInstallAnnotation] == "false"

	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	next := map[string]string{}
	var failed []string
	var firstErr error
	for _, key := range keys {
		content := cm.Data[key]
		digest := digestOf(content)
		if applied[key] == digest {
			next[key] = digest
			continue
		}
		done, err := r.applyKey(ctx, &cm, key, content, migrating)
		if err != nil {
			logger.Info("could not apply a bootstrap object; retrying", "key", key, "error", err.Error())
			r.emit(&cm, corev1.EventTypeWarning, "BootstrapApplyFailed",
				fmt.Sprintf("applying %s: %v", key, err))
			failed = append(failed, key)
			if firstErr == nil {
				firstErr = err
			}
			// What was applied before stays recorded, so a refused update is not
			// mistaken for an object that was never there.
			if old, ok := applied[key]; ok {
				next[key] = old
			}
			continue
		}
		if done {
			next[key] = digest
		}
	}

	if !recorded || !equalMaps(applied, next) {
		if err := r.record(ctx, &cm, next); err != nil {
			return ctrl.Result{}, err
		}
	}
	if firstErr != nil {
		return ctrl.Result{}, fmt.Errorf("applying %s: %w", strings.Join(failed, ", "), firstErr)
	}
	return ctrl.Result{}, nil
}

// applyKey applies one key's object. It reports whether the key is settled,
// which a skipped discovery run is as well.
func (r *Reconciler) applyKey(
	ctx context.Context, cm *corev1.ConfigMap, key, content string, migrating bool,
) (bool, error) {
	obj, err := decode(content)
	if err != nil {
		return false, err
	}
	if obj == nil {
		// A key that renders to nothing but comments has nothing to apply.
		return true, nil
	}
	if obj.GetNamespace() == "" {
		obj.SetNamespace(cm.Namespace)
	}
	if obj.GetKind() == "OperatorOps" {
		if migrating {
			var live unstructured.Unstructured
			live.SetGroupVersionKind(obj.GroupVersionKind())
			err := r.Client.Get(ctx, client.ObjectKeyFromObject(obj), &live)
			if apierrors.IsNotFound(err) {
				log.FromContext(ctx).Info("not recreating the discovery run an upgraded release had already",
					"key", key, "name", obj.GetName())
				return true, nil
			}
			if err != nil && !meta.IsNoMatchError(err) {
				return false, err
			}
		}
		if obj.GetNamespace() == cm.Namespace {
			obj.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       cm.Name,
				UID:        cm.UID,
			}})
		}
	}
	err = r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj),
		client.FieldOwner(FieldOwner), client.ForceOwnership)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return false, fmt.Errorf("%s %s is not served yet: %w", obj.GetKind(), obj.GetName(), err)
		}
		return false, fmt.Errorf("%s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
	}
	log.FromContext(ctx).Info("applied a bootstrap object", "key", key,
		"kind", obj.GetKind(), "namespace", obj.GetNamespace(), "name", obj.GetName())
	return true, nil
}

// decode parses one key into an object of an allowed kind, or nil for a key
// with no object in it.
func decode(content string) (*unstructured.Unstructured, error) {
	var m map[string]any
	if err := yaml.Unmarshal([]byte(content), &m); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	if len(m) == 0 {
		return nil, nil
	}
	obj := &unstructured.Unstructured{Object: m}
	gvk := obj.GroupVersionKind()
	if gvk.Group != group || !allowedKinds[gvk.Kind] {
		return nil, fmt.Errorf("%s is not a kind this ConfigMap may carry", obj.GetAPIVersion()+"/"+obj.GetKind())
	}
	if obj.GetName() == "" {
		return nil, errors.New("the object has no metadata.name")
	}
	return obj, nil
}

// record writes the applied digests to the ConfigMap.
func (r *Reconciler) record(ctx context.Context, cm *corev1.ConfigMap, applied map[string]string) error {
	raw, err := json.Marshal(applied)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{AppliedAnnotation: string(raw)}},
	})
	if err != nil {
		return err
	}
	return r.Client.Patch(ctx, cm, client.RawPatch(types.MergePatchType, patch))
}

func (r *Reconciler) emit(obj client.Object, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(obj, nil, eventType, reason, reason, "%s", message)
}

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// SetupWithManager watches the one ConfigMap in the operator's namespace.
//
// A failed apply is retried from one second, doubling to thirty: the common
// failure is a CRD the API server has not started serving, which clears in
// seconds, and the default limiter's ceiling of minutes would leave a fresh
// install idle long after it had.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetName() == ConfigMapName && o.GetNamespace() == r.Namespace
		}))).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](backoffBase, backoffMax),
		}).
		Named("bootstrap-objects").
		Complete(r)
}
