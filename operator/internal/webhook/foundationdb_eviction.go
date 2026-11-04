package webhook

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-v1-pod-eviction-foundationdb,mutating=false,failurePolicy=ignore,sideEffects=NoneOnDryRun,groups="",resources=pods/eviction,verbs=create,versions=v1,name=foundationdb-eviction.simplyblock.io,admissionReviewVersions=v1,timeoutSeconds=5

const (
	// fdbClusterNameLabel and fdbProcessGroupIDLabel are the labels the
	// FoundationDB operator puts on every pod it runs, naming its cluster and
	// its process group.
	fdbClusterNameLabel    = "foundationdb.org/fdb-cluster-name"
	fdbProcessGroupIDLabel = "foundationdb.org/fdb-process-group-id"
)

// foundationDBClusterGVK is the kind the FoundationDB operator reconciles. It
// is read as unstructured, as everywhere else in this operator, so the
// operator does not import the FoundationDB API.
var foundationDBClusterGVK = schema.GroupVersionKind{
	Group: "apps.foundationdb.org", Version: "v1beta2", Kind: "FoundationDBCluster",
}

// FoundationDBEvictionHandler turns the eviction of a FoundationDB pod -- what
// kubectl drain, an EKS node group update, Karpenter, and cluster-autoscaler
// all do -- into a replacement by the FoundationDB operator.
//
// Evicting a log or storage pod takes its process away before anything has
// taken over its role, and the database stops accepting commits until a
// replacement runs, which waits for the volume to move. Instead the handler
// refuses the eviction with 429, which the evicting tool retries, and adds the
// pod's process group to the cluster's spec.processGroupsToRemove. The
// FoundationDB operator then starts a replacement, excludes the old process
// once its roles and data have moved, and deletes the pod itself. The next
// retry finds the pod gone and the drain moves on.
//
// One process group is replaced at a time: while another is still on its way
// out, an eviction is refused without starting a second one.
//
// failurePolicy=Ignore: when this operator is down, evictions go through as
// they would without it -- a pause in the database, not a drain that never
// finishes. matchConditions limit the webhook to FoundationDB pod names.
type FoundationDBEvictionHandler struct {
	Client client.Client
}

func (h *FoundationDBEvictionHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx).WithValues("pod", req.Name, "namespace", req.Namespace)

	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return admission.Allowed("pod is gone")
		}
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("get pod: %w", err))
	}
	clusterName := pod.Labels[fdbClusterNameLabel]
	processGroup := pod.Labels[fdbProcessGroupIDLabel]
	if clusterName == "" || processGroup == "" {
		return admission.Allowed("not a FoundationDB process")
	}
	if pod.DeletionTimestamp != nil {
		return admission.Allowed("pod is already being deleted")
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(foundationDBClusterGVK)
	if err := h.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: clusterName}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return admission.Allowed("no FoundationDBCluster " + clusterName)
		}
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("get FoundationDBCluster: %w", err))
	}

	toRemove, _, err := unstructured.NestedStringSlice(cluster.Object, "spec", "processGroupsToRemove")
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("read processGroupsToRemove: %w", err))
	}
	present := processGroupsInStatus(cluster)

	if slices.Contains(toRemove, processGroup) {
		return retryLater(fmt.Sprintf("FoundationDB process group %s is being replaced", processGroup))
	}
	for _, other := range toRemove {
		if present[other] {
			return retryLater(fmt.Sprintf(
				"FoundationDB process group %s is waiting for %s to be replaced first", processGroup, other))
		}
	}
	if req.DryRun != nil && *req.DryRun {
		return retryLater(fmt.Sprintf("FoundationDB process group %s would be replaced", processGroup))
	}

	// Entries whose process group is gone are finished replacements; dropping
	// them keeps the list from growing with every drain.
	pending := slices.DeleteFunc(slices.Clone(toRemove), func(id string) bool { return !present[id] })
	pending = append(pending, processGroup)

	patch := client.MergeFromWithOptions(cluster.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if err := unstructured.SetNestedStringSlice(cluster.Object, pending, "spec", "processGroupsToRemove"); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if err := h.Client.Patch(ctx, cluster, patch); err != nil {
		if apierrors.IsConflict(err) {
			return retryLater("FoundationDBCluster changed meanwhile")
		}
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("mark %s for removal: %w", processGroup, err))
	}
	log.Info("eviction of a FoundationDB pod turned into a replacement", "processGroup", processGroup)
	return retryLater(fmt.Sprintf("FoundationDB process group %s is being replaced", processGroup))
}

// processGroupsInStatus is the set of process groups the FoundationDB operator
// still reports, which is every group not yet fully removed.
func processGroupsInStatus(cluster *unstructured.Unstructured) map[string]bool {
	groups, _, _ := unstructured.NestedSlice(cluster.Object, "status", "processGroups")
	present := make(map[string]bool, len(groups))
	for _, g := range groups {
		if m, ok := g.(map[string]any); ok {
			if id, ok := m["processGroupID"].(string); ok {
				present[id] = true
			}
		}
	}
	return present
}

// retryLater refuses an eviction the way a PodDisruptionBudget does: 429,
// which kubectl drain and the other evicting tools retry.
func retryLater(reason string) admission.Response {
	return admission.Response{AdmissionResponse: admissionv1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusTooManyRequests,
			Reason:  metav1.StatusReasonTooManyRequests,
			Message: reason,
		},
	}}
}
