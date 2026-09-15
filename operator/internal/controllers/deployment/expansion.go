// The expansion machine: the graph a document walks, and what each of its four
// steps does.
//
//	Validating ──► CreatingCluster ──► AwaitingCluster ──► CreatingNodes
//
// It is a Config rather than a MultiConfig, because a document has no action to
// key one on: there is one expansion and it runs once.
//
// CreatingNodes is the step that must be idempotent, and it is by construction. A
// StorageNode is identified by its cluster, its worker, and its slot, so the step
// lists what exists for the cluster and creates only the slots that do not. A
// crash part-way through creates the rest on the next pass and duplicates nothing.
//
// design-clusterdeploymentconfig.md §4.2 is the specification.

package deployment

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// configStep is the document's step type, aliased so the graph reads as the graph.
type configStep = simplyblockv1alpha2.ClusterDeploymentConfigStep

const (
	stepValidating      = simplyblockv1alpha2.ClusterDeploymentConfigStepValidating
	stepCreatingCluster = simplyblockv1alpha2.ClusterDeploymentConfigStepCreatingCluster
	stepAwaitingCluster = simplyblockv1alpha2.ClusterDeploymentConfigStepAwaitingCluster
	stepCreatingNodes   = simplyblockv1alpha2.ClusterDeploymentConfigStepCreatingNodes
)

// How long each step may take before it is reported as stuck.
//
// They differ by what the step is waiting on. Validation and the two creates are
// Kubernetes writes; awaiting the cluster is the control plane creating one, which
// is the only step here that waits on something outside Kubernetes at all.
const (
	validatingDeadline      = 5 * time.Minute
	creatingClusterDeadline = 5 * time.Minute
	awaitingClusterDeadline = 30 * time.Minute
	creatingNodesDeadline   = 10 * time.Minute
)

// expansionGraph declares the document's one state graph.
func expansionGraph() statemachine.Config[configStep] {
	deadline := func(d time.Duration) statemachine.TransitionFunc[configStep] {
		return func(context.Context, configStep, configStep) (time.Duration, error) {
			return d, nil
		}
	}
	return statemachine.Config[configStep]{
		Initial: stepValidating,
		States: map[configStep]statemachine.StateDef[configStep]{
			stepValidating: {
				To:      []configStep{stepCreatingCluster},
				OnEnter: deadline(validatingDeadline),
			},
			stepCreatingCluster: {
				To:      []configStep{stepAwaitingCluster},
				OnEnter: deadline(creatingClusterDeadline),
			},
			stepAwaitingCluster: {
				To:      []configStep{stepCreatingNodes},
				OnEnter: deadline(awaitingClusterDeadline),
			},
			stepCreatingNodes: {OnEnter: deadline(creatingNodesDeadline)},
		},
	}
}

// nextStep is the step that follows the current one. The graph is a line, so the
// first edge is the only edge.
func nextStep(machine *statemachine.Machine[configStep]) (configStep, error) {
	current := machine.CurrentState()
	for next := range machine.AllowedTransitions() {
		return next, nil
	}
	return current, fmt.Errorf("step %s declares no successor and is not terminal", current)
}

// createCluster creates the StorageCluster the document describes, or resolves the
// one it names, and refuses the two combinations §6 will not perform.
//
// It stamps the device class it read off the groups. The document carries no field
// for it, so the step takes the member every group used and writes it, where it is
// immutable from that moment. Resolving an existing cluster writes nothing: the
// cluster's class already holds.
func (r *ClusterDeploymentConfigReconciler) createCluster(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (bool, error) {
	name, err := targetClusterName(config)
	if err != nil {
		return false, err
	}

	var existing simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: config.Namespace, Name: name}
	getErr := r.Get(ctx, key, &existing)

	switch {
	case getErr == nil && config.Spec.ClusterRef == "":
		// The document asked to create a cluster and one is already there.
		// Merging would have the operator decide what a difference means, and the
		// differences that matter are of the form "this node's device list
		// changed" (§6).
		return false, refusef(ClusterExists,
			"spec.cluster.name is %s and a StorageCluster by that name already exists; "+
				"set spec.clusterRef to add nodes to it instead", name)

	case apierrors.IsNotFound(getErr) && config.Spec.ClusterRef != "":
		return false, refusef(ClusterNotFound,
			"spec.clusterRef names StorageCluster %s, which does not exist", name)

	case getErr == nil:
		// Growth against a cluster that is there. Nothing to create, and the
		// class is settled before the document existed.
		return true, r.recordCluster(ctx, config, name)

	case !apierrors.IsNotFound(getErr):
		return false, fmt.Errorf("reading StorageCluster %s: %w", name, getErr)
	}

	cluster, err := r.buildCluster(config, name)
	if err != nil {
		return false, err
	}
	if err := r.Create(ctx, cluster); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, fmt.Errorf("creating StorageCluster %s: %w", name, err)
	}
	r.emit(config, corev1.EventTypeNormal, ClusterCreated,
		fmt.Sprintf("Created StorageCluster %s", name))
	return true, r.recordCluster(ctx, config, name)
}

// buildCluster is the StorageCluster the document's template describes.
func (r *ClusterDeploymentConfigReconciler) buildCluster(
	config *simplyblockv1alpha2.ClusterDeploymentConfig, name string,
) (*simplyblockv1alpha2.StorageCluster, error) {
	template := config.Spec.Cluster
	if template == nil {
		return nil, refusef(ClusterNotFound,
			"the document neither names an existing cluster nor describes one to create")
	}

	class := deviceClassOf(config)

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: config.Namespace},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount:    template.MaxSubsystemCount,
			VCPUCount:            template.VCPUCount,
			MinHugePagesSize:     template.MinHugePagesSize,
			Stripe:               template.Stripe,
			FabricType:           template.FabricType,
			EnableFailureDomains: template.EnableFailureDomains,
			DeviceClass:          class,
			// The workload every node runs as. The document's per-group network
			// interfaces are the same for every group of a cluster in practice,
			// and the cluster is where a DaemonSet can carry them at all
			// (design-storagenode.md §5.1).
			StorageNodes: r.buildWorkload(config),
		},
	}
	return cluster, nil
}

// buildWorkload resolves spec.environment into the four distribution flags and
// carries the first group's interfaces onto the cluster.
//
// A DaemonSet is one object for every node it schedules, so its pod template
// cannot differ per group: the interfaces are the cluster's whichever group states
// them. Taking the first stated is what makes a document that repeats them in
// every group, which is how discovery writes one, mean what it looks like.
func (r *ClusterDeploymentConfigReconciler) buildWorkload(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) *simplyblockv1alpha2.StorageNodesSpec {
	workload := &simplyblockv1alpha2.StorageNodesSpec{}

	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			if workload.MgmtInterface == "" {
				workload.MgmtInterface = group.MgmtInterface
			}
			if len(workload.DataInterfaces) == 0 {
				workload.DataInterfaces = group.DataInterfaces
			}
		}
	}

	// The environment is a shorthand and this is where it is spent. Naming
	// OpenShift once decides all four, after which nothing reads the field again
	// and the nodes carry the resolved flags (§3.1).
	switch config.Spec.Environment {
	case simplyblockv1alpha2.KubernetesEnvironmentOpenShift:
		workload.OpenShiftCluster = ptr.To(true)
		workload.EnableCpuTopology = ptr.To(true)
	case simplyblockv1alpha2.KubernetesEnvironmentTalos:
		// Talos has no writable kubelet configuration and no package manager, so
		// the node applies neither.
		workload.EnableKubeletConfiguration = ptr.To(false)
	case simplyblockv1alpha2.KubernetesEnvironmentVanilla,
		simplyblockv1alpha2.KubernetesEnvironmentRancher,
		simplyblockv1alpha2.KubernetesEnvironmentK3s:
		workload.EnableKubeletConfiguration = ptr.To(true)
	}
	return workload
}

// awaitCluster waits for the control plane to have created the cluster, which is
// what status.uuid says.
func (r *ClusterDeploymentConfigReconciler) awaitCluster(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (bool, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: config.Namespace, Name: config.Status.ClusterRef}
	if err := r.Get(ctx, key, &cluster); err != nil {
		return false, fmt.Errorf("reading StorageCluster %s: %w", config.Status.ClusterRef, err)
	}
	return cluster.Status.UUID != "", nil
}

// createNodes writes one StorageNode per worker per slot, and creates only the
// slots that do not exist.
//
// The slot count comes from the cluster's own workload block, so a group of two
// workers on a two-socket layout produces four nodes. Reading it off the cluster
// rather than off the document is what makes a growth document produce nodes that
// match the fleet it is joining.
func (r *ClusterDeploymentConfigReconciler) createNodes(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (bool, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: config.Namespace, Name: config.Status.ClusterRef}
	if err := r.Get(ctx, key, &cluster); err != nil {
		return false, fmt.Errorf("reading StorageCluster %s: %w", config.Status.ClusterRef, err)
	}

	existing, err := r.nodesOfCluster(ctx, config.Namespace, cluster.Name)
	if err != nil {
		return false, err
	}

	created := append([]string(nil), config.Status.NodeRefs...)
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				for slot := int32(0); slot < slotsPerWorker(&cluster); slot++ {
					if _, there := existing[slotKey{worker: worker, slot: slot}]; there {
						continue
					}
					node := r.buildNode(config, &cluster, set, group, worker, slot)
					if err := r.Create(ctx, node); err != nil {
						if apierrors.IsAlreadyExists(err) {
							continue
						}
						return false, fmt.Errorf("creating StorageNode for worker %s slot %d: %w",
							worker, slot, err)
					}
					created = append(created, node.Name)
				}
			}
		}
	}

	sort.Strings(created)
	return true, r.recordNodes(ctx, config, created)
}

// slotKey is the identity a StorageNode has within its cluster, which is what
// makes the creation idempotent.
type slotKey struct {
	worker string
	slot   int32
}

// nodesOfCluster indexes the cluster's existing nodes by the slot each fills.
func (r *ClusterDeploymentConfigReconciler) nodesOfCluster(
	ctx context.Context, namespace, cluster string,
) (map[slotKey]struct{}, error) {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing the cluster's nodes: %w", err)
	}
	filled := map[slotKey]struct{}{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != cluster {
			continue
		}
		slot := int32(0)
		if node.Spec.Slot != nil {
			slot = *node.Spec.Slot
		}
		filled[slotKey{worker: node.Spec.WorkerNode, slot: slot}] = struct{}{}
	}
	return filled, nil
}

// slotsPerWorker is how many storage nodes a worker runs, which is one per socket
// per nodesPerSocket.
func slotsPerWorker(cluster *simplyblockv1alpha2.StorageCluster) int32 {
	workload := cluster.Spec.StorageNodes
	if workload == nil {
		return 1
	}
	sockets := int32(len(workload.SocketsToUse))
	if sockets == 0 {
		// An empty list means socket 0 alone (design-storagenode.md §5.1).
		sockets = 1
	}
	perSocket := int32(1)
	if workload.NodesPerSocket != nil && *workload.NodesPerSocket > 0 {
		perSocket = *workload.NodesPerSocket
	}
	return sockets * perSocket
}

// buildNode resolves the document's shorthands into one node.
//
// Nothing on the node refers back to the config, which is what makes the document
// safe to delete: the sizing is the cluster's, copied in, and the devices are the
// group's, expanded into one list.
func (r *ClusterDeploymentConfigReconciler) buildNode(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	cluster *simplyblockv1alpha2.StorageCluster,
	set simplyblockv1alpha2.NodeSet,
	group simplyblockv1alpha2.NodeGroup,
	worker string,
	slot int32,
) *simplyblockv1alpha2.StorageNode {
	socket, index := decomposeSlot(cluster, slot)

	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeName(cluster.Name, worker, slot),
			Namespace: config.Namespace,
			Labels: map[string]string{
				"storage.simplyblock.io/cluster": cluster.Name,
				"storage.simplyblock.io/worker":  worker,
			},
		},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: cluster.Name,
			NodeSet:    set.Name,
			WorkerNode: worker,
			SocketID:   socket,
			NodeIndex:  ptr.To(index),
			Slot:       ptr.To(slot),
			Config: simplyblockv1alpha2.StorageNodeConfig{
				// Two of the cluster's three sizing values are copied onto the
				// node, so it records the layout it was built with and the
				// operator can re-size one node at a time during a hardware
				// upgrade. maxSubsystemCount is read from the cluster instead
				// (design-storagenode.md §3.1).
				Sizing: simplyblockv1alpha2.StorageNodeSizing{
					VCPUCount:        cluster.Spec.VCPUCount,
					MinHugePagesSize: cluster.Spec.MinHugePagesSize,
				},
				DeviceNames:      devicesOf(group),
				FailureDomain:    group.FailureDomain,
				SpdkSystemMemory: group.SpdkSystemMemory,
				JournalManager:   group.JournalManager,
			},
		},
	}
}

// decomposeSlot renders a slot as the socket and the position within it, which is
// the pair a print column shows. Nothing but those columns reads either.
func decomposeSlot(
	cluster *simplyblockv1alpha2.StorageCluster, slot int32,
) (socket string, index int32) {
	workload := cluster.Spec.StorageNodes
	perSocket := int32(1)
	if workload != nil && workload.NodesPerSocket != nil && *workload.NodesPerSocket > 0 {
		perSocket = *workload.NodesPerSocket
	}

	position := slot / perSocket
	index = slot % perSocket
	if workload != nil && int(position) < len(workload.SocketsToUse) {
		return workload.SocketsToUse[position], index
	}
	return fmt.Sprintf("%d", position), index
}

// devicesOf expands a group's device selection into the one list a node carries.
// Both members expand into the same field, which takes a PCI address and a device
// path alike.
func devicesOf(group simplyblockv1alpha2.NodeGroup) []string {
	if group.Devices == nil {
		return nil
	}
	if len(group.Devices.NVMe) > 0 {
		return group.Devices.NVMe
	}
	return group.Devices.Block
}

// deviceClassOf reads the class off the document's groups, which is where it is
// stated: the device lists already say which class the deployment uses, so
// spec.cluster does not restate it.
func deviceClassOf(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) simplyblockv1alpha2.StorageClusterDeviceClass {
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			if group.Devices == nil {
				continue
			}
			if len(group.Devices.NVMe) > 0 {
				return simplyblockv1alpha2.StorageClusterDeviceClassNVMe
			}
			if len(group.Devices.Block) > 0 {
				return simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock
			}
		}
	}
	// A document whose groups name no devices at all describes a cluster whose
	// class nothing states. NVMe is what the cluster's own field defaults to, and
	// stamping it explicitly here would be inventing a fact the document does not
	// carry.
	return ""
}

// targetClusterName is the cluster the document acts on, whichever way it names
// one.
func targetClusterName(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (string, error) {
	if config.Spec.ClusterRef != "" {
		return config.Spec.ClusterRef, nil
	}
	if config.Spec.Cluster != nil && config.Spec.Cluster.Name != "" {
		return config.Spec.Cluster.Name, nil
	}
	return "", refusef(ClusterNotFound,
		"the document neither names an existing cluster nor describes one to create")
}

// nodeNameFormula names one node. A StorageNode is named for its cluster and the
// slot it fills, never for the worker, because the name has to stay stable when a
// migration re-points the node onto another host (design-storagenode.md §3.1) —
// the worker is in the name's digest rather than in its text.
var nodeNameFormula = kube.Formula{}

func nodeName(cluster, worker string, slot int32) string {
	return nodeNameFormula.Derive(cluster, worker, fmt.Sprintf("%d", slot)).Value
}

// recordCluster writes which cluster the expansion produced or joined.
func (r *ClusterDeploymentConfigReconciler) recordCluster(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig, name string,
) error {
	return r.writeStatus(ctx, config,
		func(status *simplyblockv1alpha2.ClusterDeploymentConfigStatus) {
			status.ClusterRef = name
		})
}

// recordNodes writes which nodes it created. It is a record rather than a
// dependency: nothing resolves it after the expansion.
func (r *ClusterDeploymentConfigReconciler) recordNodes(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig, names []string,
) error {
	return r.writeStatus(ctx, config,
		func(status *simplyblockv1alpha2.ClusterDeploymentConfigStatus) {
			status.NodeRefs = names
		})
}
