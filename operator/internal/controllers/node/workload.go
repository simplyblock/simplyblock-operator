// The storage-node workload, as the two reconcilers in this package touch it.
//
// A backend storage node is an SPDK process on a worker, and something has to put
// it there: a DaemonSet, a headless Service, an EndpointSlice per pod, a serving
// certificate, a ServiceAccount with its role, and a ConfigMap the init container
// reads its per-node configuration out of. The retired StorageNodeSet owned all of
// them, which is why deleting one tore the storage plane down and why the kind
// could not be retired until the ownership moved. They are children of the
// StorageCluster now, established by controller reference at the point each is
// created (§5.1).
//
// What is here is the surface the node's own reconcile and its operations ask of
// that workload: label a worker into the storage plane, take one out, ask whether
// a pod is ready, whether its per-pod DNS name is published, whether the worker's
// storage-node API answers, and hold or release an eviction. The objects
// themselves are reconciled in workload_objects.go, by the cluster that owns them.
//
// One key deliberately does not move. Every other annotation and label key in this
// product is migrating to the storage.simplyblock.io prefix (design-crd-model.md
// §7.3), and the per-slot storage-node-uuid label on a worker Node is the
// exception: Kubernetes' external-provisioner caches the set of topology keys in
// the CSINode object when the node plugin registers and hard-errors CreateVolume
// when a live Node's keys do not match the cached set, so the key must not change
// for a worker's lifetime (§5.2). The upgrade tool's key rewrite is what moves it,
// in step with the CSI driver that reads it, and until then this writes the
// spelling the driver knows.
//
// design-storagenode.md §5 is the specification.

package node

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	atlaskube "github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// storageNodeUUIDLabelPrefix is the per-slot topology label a worker Node carries,
// one key per slot, whose value is the backend node currently filling that slot.
//
// The key is scoped by cluster UUID and slot ordinal, both of which are fixed for
// the slot, while the UUID that says which backend node occupies it is the value
// and is always read fresh. That is what lets one worker host nodes of two
// simplyblock clusters without one cluster's slot colliding with the other's, and
// what makes a node replaced or relocated a change of value alone (§5.2).
const storageNodeUUIDLabelPrefix = "simplyblock.io/storage-node-uuid."

// Workload is the storage-node workload of one cluster, reachable from a
// reconciler that has a client.
//
// It carries an uncached reader beside the ordinary client for one read. A stale
// informer cache can miss a freshly published EndpointSlice, and the consequence
// is a migration that waits on DNS forever while the name has in fact resolved for
// minutes (§5.4). Every other read here is served from the cache.
type Workload struct {
	client.Client

	// Uncached reads straight from the API server. It may be nil, in which case
	// the cached client answers: a unit test with a fake client has one reader and
	// no cache to be stale.
	Uncached client.Reader

	// TLSEnabled and TLSMutualEnabled decide how the worker's storage-node API is
	// probed, because a deployment serving TLS refuses a plaintext request and a
	// refused request is not the same answer as an unreachable host.
	TLSEnabled       bool
	TLSMutualEnabled bool

	// ManagerNode is the Kubernetes node the operator itself runs on, which the
	// chart sets from spec.nodeName. It decides one question and no other:
	// whether the worker a maintenance window is draining is the manager's own,
	// in which case the manager holds its own eviction while it arranges the
	// window (selfbudget.go). Empty when nothing set it, and then it holds
	// nothing.
	ManagerNode string
}

// NodeAddress is what the control plane is given as node_address when a node
// is added or restarted.
//
// A local control plane resolves the per-pod DNS name itself, which is the
// existing precondition for both: a restart issued against a name that does
// not yet resolve fails name resolution inside the control plane, and the
// control plane's response to that is to reset the node to offline (§5.4).
//
// A managed control plane runs on a different Kubernetes cluster and can
// never resolve this cluster's own Service DNS, so it is given the worker's
// real, routable address instead -- one the storage-node-api pod already
// answers on directly, since it runs with hostNetwork (BuildStorageNodeDaemonSet).
// Reading the worker Node's own reported address is what keeps this additive:
// a deployment with no managed control plane takes exactly the path it always
// did.
func (w *Workload) NodeAddress(ctx context.Context, worker, namespace string) string {
	if address, ok := w.managedNodeAddress(ctx, worker, namespace); ok {
		return address
	}
	return utils.StorageNodeSetAPIAddress(worker, namespace)
}

// managedNodeAddress answers the worker's real address when the singleton
// ControlPlane names a managed control plane, and false otherwise -- including
// when the singleton or the worker Node cannot be read, since an operator that
// cannot tell falls back to the address that has always worked for a control
// plane this cluster hosts.
func (w *Workload) managedNodeAddress(ctx context.Context, worker, namespace string) (string, bool) {
	var cp simplyblockv1alpha2.ControlPlane
	key := client.ObjectKey{Namespace: namespace, Name: SingletonControlPlaneName}
	if err := w.Get(ctx, key, &cp); err != nil || cp.Spec.Source.Managed == nil {
		return "", false
	}

	var node corev1.Node
	if err := w.Get(ctx, client.ObjectKey{Name: worker}, &node); err != nil {
		return "", false
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
			return fmt.Sprintf("%s:5000", addr.Address), true
		}
	}
	return "", false
}

// LabelWorker puts one worker into a cluster's storage plane and rewrites the
// per-slot labels of every node on it.
//
// Rebuilding the label set from a failed List is the failure mode that matters: an
// empty result read as "no slots are desired" would delete every storage-node-uuid
// label from every worker and break CSI topology across the cluster. The reconcile
// aborts on a List error rather than proceeding with a partial view (§5.2).
func (w *Workload) LabelWorker(ctx context.Context, namespace, cluster, worker string) error {
	desired, err := w.desiredSlotLabels(ctx, namespace, cluster, worker)
	if err != nil {
		return err
	}
	return w.applyWorkerLabels(ctx, namespace, cluster, worker, desired)
}

// ReleaseWorker removes a cluster's storage-plane labels from the worker a node
// has just left, and only when no other node of that cluster remains on it.
//
// Removing them from a worker that still hosts a node would unschedule it, which
// is why the emptiness check is here rather than at the call site: a relocation
// off a two-socket host leaves the second socket behind.
func (w *Workload) ReleaseWorker(
	ctx context.Context, namespace, cluster, movedNode string,
) error {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := w.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list the cluster's nodes: %w", err)
	}

	// The moved node's object already names the worker it went to, so the worker
	// it left is simply one that carries the cluster's label and that no node of
	// the cluster is on any more. Counting the occupied workers and stripping the
	// rest finds it without having to remember where the node used to be.
	occupied := map[string]struct{}{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != cluster {
			continue
		}
		occupied[node.Spec.WorkerNode] = struct{}{}
	}

	var workers corev1.NodeList
	if err := w.List(ctx, &workers); err != nil {
		return fmt.Errorf("list the Kubernetes workers: %w", err)
	}
	for i := range workers.Items {
		worker := &workers.Items[i]
		if _, still := occupied[worker.Name]; still {
			continue
		}
		if !w.carriesStoragePlane(worker, cluster) {
			continue
		}
		if err := w.stripStoragePlane(ctx, worker, cluster); err != nil {
			return err
		}
	}
	return nil
}

// desiredSlotLabels is the per-slot label set one worker should carry for one
// cluster: a key per slot that has come online at least once, valued with the
// backend node filling it.
func (w *Workload) desiredSlotLabels(
	ctx context.Context, namespace, cluster, worker string,
) (map[string]string, error) {
	var clusterObject simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: namespace, Name: cluster}
	if err := w.Get(ctx, key, &clusterObject); err != nil {
		return nil, fmt.Errorf("read cluster %s: %w", cluster, err)
	}
	if clusterObject.Status.UUID == "" {
		// A cluster the control plane has not created has no UUID, so the keys its
		// nodes will carry are not derivable yet. The DaemonSet selector is still
		// applied, which is what gets a pod onto the worker in the first place.
		return map[string]string{}, nil
	}

	var nodes simplyblockv1alpha2.StorageNodeList
	if err := w.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list the cluster's nodes: %w", err)
	}

	desired := map[string]string{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != cluster || node.Spec.WorkerNode != worker {
			continue
		}
		if node.Status.UUID == "" {
			continue
		}
		slot := int32(0)
		if node.Spec.Slot != nil {
			slot = *node.Spec.Slot
		}
		desired[fmt.Sprintf("%s.%d", clusterObject.Status.UUID, slot)] = node.Status.UUID
	}
	return desired, nil
}

// applyWorkerLabels writes the DaemonSet selector and the slot labels onto one
// worker, and removes the slot keys of this cluster that are no longer wanted.
//
// A slot key belonging to another simplyblock cluster is left alone: a worker may
// host nodes of more than one, and this pass owns only the cluster it was called
// for.
func (w *Workload) applyWorkerLabels(
	ctx context.Context, namespace, cluster, worker string, desired map[string]string,
) error {
	var node corev1.Node
	if err := w.Get(ctx, client.ObjectKey{Name: worker}, &node); err != nil {
		return fmt.Errorf("read worker %s: %w", worker, err)
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}

	var clusterObject simplyblockv1alpha2.StorageCluster
	if err := w.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cluster}, &clusterObject); err != nil {
		return fmt.Errorf("read cluster %s: %w", cluster, err)
	}

	changed := false
	if node.Labels[atlaskube.LabelStorageNodeSet] != cluster {
		node.Labels[atlaskube.LabelStorageNodeSet] = cluster
		changed = true
	}

	if uuid := clusterObject.Status.UUID; uuid != "" {
		for key, value := range node.Labels {
			if !strings.HasPrefix(key, storageNodeUUIDLabelPrefix) {
				continue
			}
			slot := strings.TrimPrefix(key, storageNodeUUIDLabelPrefix)
			separator := strings.LastIndex(slot, ".")
			if separator < 0 || slot[:separator] != uuid {
				continue
			}
			if desired[slot] != value {
				delete(node.Labels, key)
				changed = true
			}
		}
		for slot, uuid := range desired {
			key := storageNodeUUIDLabelPrefix + slot
			if node.Labels[key] != uuid {
				node.Labels[key] = uuid
				changed = true
			}
		}
	}

	if !changed {
		return nil
	}
	return w.Update(ctx, &node)
}

// carriesStoragePlane reports whether a worker is labeled into this cluster's
// storage plane.
func (w *Workload) carriesStoragePlane(worker *corev1.Node, cluster string) bool {
	return worker.Labels[atlaskube.LabelStorageNodeSet] == cluster
}

// stripStoragePlane takes a worker out of the storage plane, selector and slot
// keys together.
func (w *Workload) stripStoragePlane(
	ctx context.Context, worker *corev1.Node, cluster string,
) error {
	delete(worker.Labels, atlaskube.LabelStorageNodeSet)
	for key := range worker.Labels {
		if strings.HasPrefix(key, storageNodeUUIDLabelPrefix) {
			delete(worker.Labels, key)
		}
	}
	if err := w.Update(ctx, worker); err != nil {
		return fmt.Errorf("remove worker %s from cluster %s's storage plane: %w",
			worker.Name, cluster, err)
	}
	return nil
}

// PodReady reports whether the storage-node pod on one worker is running and
// ready.
func (w *Workload) PodReady(
	ctx context.Context, namespace, cluster, worker string,
) (bool, error) {
	pod, found, err := w.podOn(ctx, namespace, cluster, worker)
	if err != nil || !found {
		return false, err
	}
	if pod.Status.Phase != corev1.PodRunning {
		return false, nil
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue, nil
		}
	}
	return false, nil
}

// PodGone reports whether the storage-node pod has left the worker, which is what
// a maintenance window waits for once it has relaxed the budget.
func (w *Workload) PodGone(
	ctx context.Context, namespace, cluster, worker string,
) (bool, error) {
	_, found, err := w.podOn(ctx, namespace, cluster, worker)
	return !found, err
}

// podOn finds the storage-node pod scheduled onto one worker.
func (w *Workload) podOn(
	ctx context.Context, namespace, cluster, worker string,
) (*corev1.Pod, bool, error) {
	var pods corev1.PodList
	err := w.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{
		atlaskube.LabelApp:            atlaskube.AppStorageNode,
		atlaskube.LabelStorageNodeSet: cluster,
	})
	if err != nil {
		return nil, false, fmt.Errorf("list the cluster's storage-node pods: %w", err)
	}
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName == worker && pods.Items[i].DeletionTimestamp.IsZero() {
			return &pods.Items[i], true, nil
		}
	}
	return nil, false, nil
}

// PublishedInDNS reports whether the worker's per-pod DNS name is in the headless
// Service's EndpointSlice, which is what the control plane resolves node_address
// through.
//
// The read is uncached for the reason the type's doc comment states.
func (w *Workload) PublishedInDNS(
	ctx context.Context, namespace, cluster, worker string,
) (bool, error) {
	reader := client.Reader(w.Client)
	if w.Uncached != nil {
		reader = w.Uncached
	}

	var slice discoveryv1.EndpointSlice
	key := client.ObjectKey{
		Namespace: namespace,
		Name:      atlaskube.StorageNodeSetAPIEndpointSliceName(cluster),
	}
	if err := reader.Get(ctx, key, &slice); err != nil {
		if apierrors.IsNotFound(err) {
			// The slice is written by the same reconcile that labels the worker,
			// so its absence is "not yet" rather than an error.
			return false, nil
		}
		return false, fmt.Errorf("read the storage-node API EndpointSlice: %w", err)
	}

	wanted := utils.NodeHostnameLabel(worker)
	for i := range slice.Endpoints {
		endpoint := &slice.Endpoints[i]
		if endpoint.Hostname != nil && *endpoint.Hostname == wanted &&
			len(endpoint.Addresses) > 0 {
			return true, nil
		}
	}

	// Which of the two it is matters, and the two have different causes: a worker
	// absent from the slice means it has not been published, and one present
	// without an address means the pod has no IP yet.
	published := make([]string, 0, len(slice.Endpoints))
	for i := range slice.Endpoints {
		name := "<unnamed>"
		if slice.Endpoints[i].Hostname != nil {
			name = *slice.Endpoints[i].Hostname
		}
		published = append(published, fmt.Sprintf("%s=%v", name, slice.Endpoints[i].Addresses))
	}
	logf.FromContext(ctx).V(1).Info("the target worker is not published in the storage-node API EndpointSlice",
		"want", wanted, "resourceVersion", slice.ResourceVersion, "published", published)
	return false, nil
}

// HostAnswers reports whether the worker's storage-node API is reachable, which is
// the one read in this package with no streamed counterpart: it is a Kubernetes-
// side check against a pod rather than a control-plane object (§4.4).
func (w *Workload) HostAnswers(ctx context.Context, namespace, worker string) (bool, error) {
	if err := utils.StorageNodeAPIReachable(
		ctx, worker, namespace, w.TLSEnabled, w.TLSMutualEnabled,
	); err != nil {
		logf.FromContext(ctx).V(1).Info("the worker's storage-node API does not answer yet",
			"worker", worker, "err", err.Error())
		return false, nil
	}
	return true, nil
}

// BlockEviction labels the worker's storage pod and creates a budget that allows
// no disruption, so `kubectl drain` blocks on it while the backend node is being
// taken down gracefully (§10).
func (w *Workload) BlockEviction(
	ctx context.Context, namespace, cluster, worker string,
) error {
	pod, found, err := w.podOn(ctx, namespace, cluster, worker)
	if err != nil {
		return err
	}
	if !found {
		// Nothing to hold. The pod has already gone, which is the state Releasing
		// waits for, so the window is further along than it thought.
		return nil
	}
	if pod.Labels[maintenanceLabel] != worker {
		patch := client.MergeFrom(pod.DeepCopy())
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels[maintenanceLabel] = worker
		if err := w.Patch(ctx, pod, patch); err != nil {
			return fmt.Errorf("label the storage pod on worker %s: %w", worker, err)
		}
	}
	return w.setBudget(ctx, namespace, cluster, worker, 0)
}

// AllowEviction relaxes the budget to permit the one eviction the drain is waiting
// on.
func (w *Workload) AllowEviction(
	ctx context.Context, namespace, cluster, worker string,
) error {
	return w.setBudget(ctx, namespace, cluster, worker, 1)
}

// ClearEvictionBudget removes the budget and the label the window put in place, so
// the worker is drainable by the ordinary rules again.
//
// A budget left behind by a crashed operator is what would make a worker
// undrainable forever, which is why this runs from the window's terminal step
// rather than only from its success.
func (w *Workload) ClearEvictionBudget(
	ctx context.Context, namespace, cluster, worker string,
) error {
	budget := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{
		Name:      maintenanceBudgetName(cluster, worker),
		Namespace: namespace,
	}}
	if err := w.Delete(ctx, budget); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the maintenance budget for worker %s: %w", worker, err)
	}

	pod, found, err := w.podOn(ctx, namespace, cluster, worker)
	if err != nil || !found {
		return err
	}
	if _, labeled := pod.Labels[maintenanceLabel]; !labeled {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	delete(pod.Labels, maintenanceLabel)
	if err := w.Patch(ctx, pod, patch); err != nil {
		return fmt.Errorf("unlabel the storage pod on worker %s: %w", worker, err)
	}
	return nil
}

// setBudget creates or updates the per-worker budget with the given allowance.
func (w *Workload) setBudget(
	ctx context.Context, namespace, cluster, worker string, allowed int32,
) error {
	desired := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maintenanceBudgetName(cluster, worker),
			Namespace: namespace,
			Labels: map[string]string{
				atlaskube.LabelApp:            atlaskube.AppStorageNode,
				atlaskube.LabelStorageNodeSet: cluster,
			},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: allowed},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{maintenanceLabel: worker},
			},
		},
	}

	var existing policyv1.PodDisruptionBudget
	key := client.ObjectKeyFromObject(desired)
	err := w.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		if err := w.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create the maintenance budget for worker %s: %w", worker, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the maintenance budget for worker %s: %w", worker, err)
	}

	if existing.Spec.MaxUnavailable != nil && existing.Spec.MaxUnavailable.IntVal == allowed {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.MaxUnavailable = desired.Spec.MaxUnavailable
	existing.Spec.Selector = desired.Spec.Selector
	if err := w.Patch(ctx, &existing, patch); err != nil {
		return fmt.Errorf("relax the maintenance budget for worker %s: %w", worker, err)
	}
	return nil
}

// maintenanceLabel marks the one pod a maintenance window's budget selects. It is
// the worker's own name rather than a fixed value, so two windows on two workers
// each hold their own pod and neither budget selects the other's.
const maintenanceLabel = "storage.simplyblock.io/maintenance-worker"

// maintenanceBudgetName is the budget of one window. It is derived through
// atlas-lib's formula so that two long worker names cannot collide on one object
// name, which is what would make a window relax somebody else's budget.
var maintenanceBudgetFormula = atlaskube.Formula{Prefix: "sb-maintenance-"}

func maintenanceBudgetName(cluster, worker string) string {
	return maintenanceBudgetFormula.Derive(cluster, worker).Value
}
