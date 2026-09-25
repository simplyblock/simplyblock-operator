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
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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
	stepActivating      = simplyblockv1alpha2.ClusterDeploymentConfigStepActivating
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

	// activatingDeadline covers every node this document created coming online,
	// which is a node add per worker and the cap serializes them. It is the
	// longest step for that reason rather than because activating is slow.
	activatingDeadline = 60 * time.Minute
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
			stepCreatingNodes: {
				To:      []configStep{stepActivating},
				OnEnter: deadline(creatingNodesDeadline),
			},
			stepActivating: {OnEnter: deadline(activatingDeadline)},
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
	case getErr == nil && config.Status.ClusterRef == name:
		// This document created it on an earlier pass and said so. Re-entering a
		// step is ordinary — a lost status write, a generation bump, a restart
		// mid-expansion — so a step that refused its own prior success would fail
		// the deployment on a retry rather than resume it.
		return true, r.recordCluster(ctx, config, name)

	case getErr == nil && config.Spec.ClusterRef == "":
		// The document asked to create a cluster and one is already there that it
		// did not put there. Merging would have the operator decide what a
		// difference means, and the differences that matter are of the form "this
		// node's device list changed" (§6).
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
	if err := r.Create(ctx, cluster); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if config.Status.ClusterRef == name {
				// The read above was served from a cache that had not caught up
				// with this document's own earlier create. The same resumption as
				// at the top of the switch, reached by the other route.
				return true, r.recordCluster(ctx, config, name)
			}
			// The read above missed and somebody created the cluster between the
			// two. That is the ClusterExists case arriving by a different route,
			// not a success: the document asked to create a cluster and did not,
			// and nothing here proves the one that is there is the one it
			// described (§6).
			return false, refusef(ClusterExists,
				"spec.cluster.name is %s and a StorageCluster by that name was created "+
					"while this document was being expanded; set spec.clusterRef to add "+
					"nodes to it instead", name)
		}
		return false, fmt.Errorf("creating StorageCluster %s: %w", name, err)
	}
	r.emit(config, corev1.EventTypeNormal, ClusterCreated,
		fmt.Sprintf("Created StorageCluster %s", name))
	return true, r.recordCluster(ctx, config, name)
}

// activateCluster waits for the nodes this document created and then asks for
// the cluster to be activated.
//
// The wait is over status.nodeRefs rather than over whatever nodes happen to
// name the cluster, because the document is answering for what it built: a node
// somebody else added later is not one this deployment is waiting on, and a node
// this deployment made that never came up is one it must not pass over.
//
// The activation is a StorageClusterOps like any other, raised by name so that
// re-entering the step finds the one it raised rather than asking twice. What
// happens to it afterward is that operation's business; this document has
// described a deployment and asked for it, which is where its own job ends.
func (r *ClusterDeploymentConfigReconciler) activateCluster(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (bool, error) {
	if config.Status.ClusterRef == "" {
		return false, refusef(ClusterNotFound,
			"the document records no cluster to activate")
	}

	for _, name := range config.Status.NodeRefs {
		var node simplyblockv1alpha2.StorageNode
		key := client.ObjectKey{Namespace: config.Namespace, Name: name}
		if err := r.Get(ctx, key, &node); err != nil {
			if apierrors.IsNotFound(err) {
				// A node the document created and somebody has since removed is
				// not one to wait for. The deployment it described is what it
				// built, and this is no longer part of it.
				continue
			}
			return false, fmt.Errorf("reading StorageNode %s: %w", name, err)
		}
		if node.Status.Phase != simplyblockv1alpha2.StorageNodePhaseOnline {
			r.emit(config, corev1.EventTypeNormal, AwaitingNodes, fmt.Sprintf(
				"StorageNode %s is %s rather than Online", name, node.Status.Phase))
			return false, nil
		}
	}

	name := config.Status.ClusterRef + "-activate"
	ops := &simplyblockv1alpha2.StorageClusterOps{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: config.Namespace},
		Spec: simplyblockv1alpha2.StorageClusterOpsSpec{
			ClusterRef: config.Status.ClusterRef,
			Action:     simplyblockv1alpha2.StorageClusterOpsActionActivate,
		},
	}
	if err := controllerutil.SetControllerReference(config, ops, r.Scheme); err != nil {
		return false, err
	}
	if err := r.Create(ctx, ops); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("asking for cluster %s to be activated: %w",
				config.Status.ClusterRef, err)
		}
		// Raised on an earlier pass, which is the step being re-entered rather
		// than anything having gone wrong.
		return true, nil
	}

	r.emit(config, corev1.EventTypeNormal, ActivationRequested, fmt.Sprintf(
		"Every node is online, so cluster %s was asked to activate",
		config.Status.ClusterRef))
	return true, nil
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

	class := DeviceClassOf(config)

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: config.Namespace},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: template.MaxSubsystemCount,
			VCPUCount:         template.VCPUCount,
			// Both are immutable on the cluster, so this is the only moment
			// either can be set at all. Copied as stated, nil included: an
			// unstated setting leaves the cluster's own default to decide
			// rather than having the expansion invent one.
			EnableChecksumValidation: template.EnableChecksumValidation,
			EnableAtomicity4K:        template.EnableAtomicity4K,
			MinHugePagesSize:         template.MinHugePagesSize,
			Stripe:                   template.Stripe,
			FabricType:               template.FabricType,
			EnableFailureDomains:     template.EnableFailureDomains,
			KMS:                      template.KMS,
			DeviceClass:              class,
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
	if template := config.Spec.Cluster; template != nil {
		// The socket layout decides how many storage nodes a worker runs, and
		// CreatingNodes reads it back off the cluster. Leaving it unset here gave
		// every cluster a document created one node on socket 0, whatever the
		// document said.
		workload.SocketsToUse = template.SocketsToUse
		workload.NodesPerSocket = template.NodesPerSocket
		// How many workers the node controller may add at once. Left here, a
		// document that stated a budget was expanded into a cluster carrying the
		// default of one, and the deployment it described was added serially.
		// Unstated stays unstated, so the cluster's own default decides it.
		workload.NodeProvisioningBudget = template.NodeProvisioningBudget
		// The document says a drive is to be formatted; this is where that is
		// resolved to how, because the how is not the same operation twice. An
		// NVMe device is reformatted to a 4K block size by the control plane at
		// node-add, and a logical block device has its signatures wiped on the
		// worker before the node is added. Setting the NVMe field for a block
		// cluster asked a control plane to reformat a namespace the cluster does
		// not have, and left the wipe undone.
		if DeviceClassOf(config) == simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock {
			workload.EnableBlockFormat = template.EnableDriveFormat
		} else {
			workload.EnableFormat4K = template.EnableDriveFormat
		}
		// The journal layout is the cluster's and immutable on it, so the
		// document is the only place it can still be stated. Dropping it here
		// partitioned a journal out of every drive on a deployment reviewed for
		// a dedicated one.
		workload.EnableJournalDevice = template.EnableJournalDevice
	}

	// The cluster slot of spec.images. It is read here rather than in
	// buildCluster because the field it fills is on the workload, and it is
	// spent only on this path: a document naming an existing cluster in
	// clusterRef never reaches buildCluster at all, so the slot is ignored for
	// the same reason spec.cluster is.
	if images := config.Spec.Images; images != nil && images.Cluster != nil {
		workload.Image = images.Cluster.Image
		workload.ImagePullPolicy = images.Cluster.ImagePullPolicy
	}

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
		// Stated rather than left nil. The renderer reads an unset flag as skipping
		// the kubelet configuration, and the settings this product has shipped
		// for OpenShift all configure it, so silence here would change what an
		// OpenShift deployment does.
		workload.EnableKubeletConfiguration = ptr.To(true)
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

	// The record is rebuilt from the slots the document describes rather than
	// accumulated across passes. A pass that created a node and then failed to
	// persist status.nodeRefs would otherwise skip it on the next pass, as one
	// that already exists, and the finished document would permanently omit a
	// node it created.
	var created []string
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				for slot := int32(0); slot < slotsPerWorker(&cluster); slot++ {
					if name, there := existing[slotKey{worker: worker, slot: slot}]; there {
						created = append(created, name)
						continue
					}
					node := r.buildNode(config, &cluster, set, group, worker, slot)
					if err := r.Create(ctx, node); err != nil {
						if apierrors.IsAlreadyExists(err) {
							created = append(created, node.Name)
							continue
						}
						return false, fmt.Errorf("creating StorageNode for worker %s slot %d: %w",
							worker, slot, err)
					}
					// Counted here rather than from the length of the record
					// below, which is rebuilt every pass and holds the nodes that
					// were already there as well.
					countNodesCreated(config.Namespace, 1)
					created = append(created, node.Name)
				}
			}
		}
	}

	sort.Strings(created)
	created = slices.Compact(created)
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
) (map[slotKey]string, error) {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing the cluster's nodes: %w", err)
	}
	filled := map[slotKey]string{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != cluster {
			continue
		}
		slot := int32(0)
		if node.Spec.Slot != nil {
			slot = *node.Spec.Slot
		}
		filled[slotKey{worker: node.Spec.WorkerNode, slot: slot}] = node.Name
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
	return slotsOf(workload.SocketsToUse, workload.NodesPerSocket)
}

// slotsOf is the same arithmetic against the fields themselves, because the
// document states the layout on its cluster template and the cluster states it
// on its node workload, and a deployment counted by one rule and built by
// another is a deployment whose validation means nothing.
func slotsOf(socketsToUse []string, nodesPerSocket *int32) int32 {
	sockets := int32(len(socketsToUse))
	if sockets == 0 {
		// An empty list means socket 0 alone (design-storagenode.md §5.1).
		sockets = 1
	}
	perSocket := int32(1)
	if nodesPerSocket != nil && *nodesPerSocket > 0 {
		perSocket = *nodesPerSocket
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
				// A node joining a cluster that already exists is an expansion,
				// which the control plane reads as a request to rebalance onto it
				// rather than to treat it as part of an initial layout. A growth
				// document is exactly that case.
				Expand:           expansionOf(config),
				DeviceNames:      devicesOf(group),
				FailureDomain:    group.FailureDomain,
				SpdkSystemMemory: group.SpdkSystemMemory,
				JournalManager:   group.JournalManager,
				// The two SPDK slots of spec.images. They are the document's
				// statement for the whole fleet and land per node, because the
				// fields are per node so that a later rollout can walk it one
				// machine at a time.
				SpdkImage:                imageOf(config, spdkSlot),
				SpdkImagePullPolicy:      pullPolicyOf(config, spdkSlot),
				SpdkProxyImage:           imageOf(config, spdkProxySlot),
				SpdkProxyImagePullPolicy: pullPolicyOf(config, spdkProxySlot),
			},
		},
	}
}

// The two slots of spec.images a node is built from, as accessors rather than as
// a switch, so that a slot added later is one function and not a case in four.
func spdkSlot(images *simplyblockv1alpha2.DeploymentImages) *simplyblockv1alpha2.ImageSpec {
	return images.SPDK
}

func spdkProxySlot(images *simplyblockv1alpha2.DeploymentImages) *simplyblockv1alpha2.ImageSpec {
	return images.SPDKProxy
}

// imageOf and pullPolicyOf read one slot, or the zero value when the document
// states no images or not that slot. The zero value is what the expansion writes
// for an unstated slot, so the field downstream keeps its own default rather than
// being overridden with nothing.
func imageOf(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	slot func(*simplyblockv1alpha2.DeploymentImages) *simplyblockv1alpha2.ImageSpec,
) string {
	if spec := imageSlot(config, slot); spec != nil {
		return spec.Image
	}
	return ""
}

func pullPolicyOf(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	slot func(*simplyblockv1alpha2.DeploymentImages) *simplyblockv1alpha2.ImageSpec,
) corev1.PullPolicy {
	if spec := imageSlot(config, slot); spec != nil {
		return spec.ImagePullPolicy
	}
	return ""
}

func imageSlot(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	slot func(*simplyblockv1alpha2.DeploymentImages) *simplyblockv1alpha2.ImageSpec,
) *simplyblockv1alpha2.ImageSpec {
	if config.Spec.Images == nil {
		return nil
	}
	return slot(config.Spec.Images)
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

// expansionOf reports whether the nodes this document creates are joining a
// cluster that already exists.
//
// The control plane reads the flag as a request to rebalance onto the new node
// rather than to treat it as part of an initial layout, so a growth document's
// nodes have to carry it: they are by definition an addition to an active
// cluster. A document that creates its own cluster is the initial layout, and
// leaves it unset.
func expansionOf(config *simplyblockv1alpha2.ClusterDeploymentConfig) *bool {
	if config.Spec.ClusterRef == "" {
		return nil
	}
	return ptr.To(true)
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

// DeviceClassOf reads the class off the document's groups, which is where it is
// stated: the device lists already say which class the deployment uses, so
// spec.cluster does not restate it.
//
// It is exported because admission asks the same question before the document
// becomes immutable (design-clusterdeploymentconfig.md §5.1), and a second
// reading of the groups would be free to disagree with this one.
func DeviceClassOf(
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

// TargetClusterName is the cluster the document acts on, whichever way it names
// one, and empty for a document that names none. It is exported for the reason
// DeviceClassOf is.
func TargetClusterName(config *simplyblockv1alpha2.ClusterDeploymentConfig) string {
	if config.Spec.ClusterRef != "" {
		return config.Spec.ClusterRef
	}
	if config.Spec.Cluster != nil {
		return config.Spec.Cluster.Name
	}
	return ""
}

// targetClusterName is TargetClusterName as the expansion needs it, where a
// document naming no cluster is a refusal rather than an empty string.
func targetClusterName(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (string, error) {
	if name := TargetClusterName(config); name != "" {
		return name, nil
	}
	return "", refusef(ClusterNotFound,
		"the document neither names an existing cluster nor describes one to create")
}

// nodeNameFormula names one node, from the cluster, the worker, and the slot
// (design-storagenode.md §3.1). All three are in the name's text while it fits and
// in its digest once the limit below forces a truncation, so two nodes differing
// only by worker never collide either way.
//
// The name is stable across a migration because Kubernetes never renames an
// object, not because the worker is kept out of it: a migration re-points
// spec.workerNode on the node that exists. So a node built on one worker and
// migrated to another keeps a name describing where it was built, and
// spec.workerNode rather than the name is where the current host is read.
//
// The limit is a label's 63 bytes and not the 253 an object name may be, because
// the name travels: the StorageDevice mirror writes it into
// storage.simplyblock.io/node on every device of the node. §19.1 of
// design-api-upgrade.md is the rule, and the arithmetic is not academic — a
// regional cluster name and a worker a cloud named after its fully qualified
// domain name are 68 bytes between them, so the overflow is what ordinary inputs
// produce rather than what a long one does.
//
// Shortening the limit does not strand the nodes of a cluster that already has
// some. A name that fitted the wider limit is returned unchanged whenever it also
// fits this one, and createNodes finds what exists by the worker and slot its spec
// records rather than by re-deriving the name — which is also why idempotent
// re-expansion rests on the spec rather than on this formula.
var nodeNameFormula = kube.Formula{Limit: kube.MaxLabelValueLength}

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
