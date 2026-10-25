// The preflight gate that holds a node on a network until a probe pod, attached
// as the SPDK pod will be, shows the attach works. The probe is deleted before
// the node leaves the step, so it and the SPDK pod never hold an address together.
//
// Specified by operator/docs/designs/design-storage-network.md §5.4, §8.1,
// and §8.2.

package node

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/multus"
	"github.com/simplyblock/simplyblock-operator/internal/netprobe"
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=create;delete

// maxProbePeers bounds the peers one interface pings, which bounds how long the
// probe runs on a cluster with many nodes.
const maxProbePeers = 3

// declaresDefinitions reports whether the declaration attaches any network
// through a definition, which is what a probe has something to check for.
func declaresDefinitions(network *simplyblockv1alpha2.StorageNetworkSpec) bool {
	return len(pooledNetworks(network)) > 0
}

// checkNetwork is the gate. A cluster that attaches no network through a
// definition passes at once. Otherwise, every referenced definition has to exist,
// and the probe pod has to attach, hold the declared addresses, find them free,
// and reach its peers.
func (r *StorageNodeReconciler) checkNetwork(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) (nodeStep, bool, error) {
	network := networkOf(cluster)
	if !declaresDefinitions(network) {
		return stepAwaitingSlot, true, nil
	}

	if err := r.requireDefinitions(ctx, node, cluster, network); err != nil {
		return stepCheckingNetwork, false, err
	}
	if err := r.assignAddresses(ctx, node, cluster, network); err != nil {
		return stepCheckingNetwork, false, err
	}
	attachments, err := BuildAttachments(node.Namespace, network, assignedAddresses(node.Status.Network))
	if err != nil {
		return stepCheckingNetwork, false, r.heldFor(cluster, NetworkAttachmentFailed, "%v", err)
	}

	pod, found, err := r.probePod(ctx, node, netprobe.KindGuest)
	if err != nil {
		return stepCheckingNetwork, false, err
	}
	if !found {
		return stepCheckingNetwork, false, r.startProbe(ctx, node, cluster, attachments)
	}

	// A probe that is being deleted belongs to an earlier attempt, and under the
	// same name it would be read as this one's. Its address may no longer be the
	// node's, so it is waited out and never judged.
	if pod.DeletionTimestamp != nil {
		return stepCheckingNetwork, false, nil
	}
	if probeFinished(pod) {
		return r.readProbe(ctx, node, cluster, pod, attachments)
	}
	return r.waitForProbe(ctx, cluster, pod)
}

// probeFinished reports whether the guest probe's checks have ended: the checking
// container has terminated, or the pod as a whole has failed. The holder keeps the
// pod running after the checks, so the pod's phase is not the signal.
func probeFinished(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return true
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == netprobe.CheckContainer && status.State.Terminated != nil {
			return true
		}
	}
	return false
}

// heldFor counts the failure and returns the error that holds the node with its
// reason.
func (r *StorageNodeReconciler) heldFor(
	cluster *simplyblockv1alpha2.StorageCluster, reason, format string, args ...any,
) error {
	recordAttachFailure(cluster.Name, reason)
	return blockedf(reason, format, args...)
}

// requireDefinitions holds the node when a referenced definition cannot be read:
// the cluster serves no such kind, or the named definition does not exist.
func (r *StorageNodeReconciler) requireDefinitions(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster, network *simplyblockv1alpha2.StorageNetworkSpec,
) error {
	for _, n := range pooledNetworks(network) {
		ref := n.Attachment.NetworkAttachmentRef
		_, err := multus.GetDefinition(ctx, r.Client, client.ObjectKey{Namespace: node.Namespace, Name: ref.Name})
		switch {
		case err == nil:
		case multus.KindNotServed(err):
			return r.heldFor(cluster, NetworkAttachmentUnsupported,
				"the cluster serves no NetworkAttachmentDefinition kind, so Multus is not installed")
		case apierrors.IsNotFound(err):
			return r.heldFor(cluster, NetworkAttachmentNotFound,
				"the NetworkAttachmentDefinition %s/%s for network %q does not exist",
				node.Namespace, ref.Name, n.Name)
		default:
			return fmt.Errorf("reading the definition %q: %w", ref.Name, err)
		}
	}
	return nil
}

// probePod reads the node's probe pod of one kind.
func (r *StorageNodeReconciler) probePod(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, kind string,
) (*corev1.Pod, bool, error) {
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: node.Namespace, Name: netprobe.Name(node.Name, kind)}
	if err := r.Get(ctx, key, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading the %s probe: %w", kind, err)
	}
	return &pod, true, nil
}

// startProbe creates the guest probe. Its name follows from the node, so a pass
// that runs again finds it, and a create that races another pass is answered by
// AlreadyExists and treated as having succeeded.
func (r *StorageNodeReconciler) startProbe(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster, attachments []Attachment,
) error {
	if r.ProbeImage == "" {
		return fmt.Errorf("the network probe has no image: the operator's own is set by SB_OPERATOR_IMAGE")
	}
	annotation, err := AnnotationValue(attachments)
	if err != nil {
		return err
	}
	checks, err := r.probeChecks(ctx, node, networkOf(cluster), attachments)
	if err != nil {
		return err
	}

	pod, err := netprobe.GuestPod(netprobe.GuestOptions{
		Namespace: node.Namespace, Node: node.Name, Worker: node.Spec.WorkerNode,
		Image: r.ProbeImage, Owner: ownerOf(node), Networks: annotation,
		Checks: checks,
	})
	if err != nil {
		return r.heldFor(cluster, NetworkAttachmentFailed, "building the network probe: %v", err)
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating the network probe: %w", err)
	}
	return nil
}

// probeChecks is what the probe checks on each interface: that its address is
// free, which of the next addresses of the network's pool are free, and that the
// cluster's online nodes on the same network answer.
func (r *StorageNodeReconciler) probeChecks(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	network *simplyblockv1alpha2.StorageNetworkSpec, attachments []Attachment,
) ([]netprobe.InterfaceCheck, error) {
	var siblings simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &siblings, client.InNamespace(node.Namespace),
		client.MatchingFields{clusterRefField: node.Spec.ClusterRef}); err != nil {
		return nil, fmt.Errorf("listing the cluster's nodes: %w", err)
	}

	checks := make([]netprobe.InterfaceCheck, 0, len(attachments))
	for _, a := range attachments {
		own, err := netip.ParsePrefix(a.IPs[0])
		if err != nil {
			return nil, fmt.Errorf("the address %q on network %q: %w", a.IPs[0], a.Network, err)
		}
		var peers []string
		for i := range siblings.Items {
			sibling := &siblings.Items[i]
			// Only a node that is online has anything listening on its address. The
			// first node of a cluster would otherwise wait forever on peers that
			// are not added until it is.
			if sibling.Name == node.Name || sibling.Status.Phase != simplyblockv1alpha2.StorageNodePhaseOnline {
				continue
			}
			address := assignedAddresses(sibling.Status.Network)[a.Network]
			if address == "" {
				continue
			}
			if prefix, err := netip.ParsePrefix(address); err == nil {
				peers = append(peers, prefix.Addr().String())
			}
		}
		slices.Sort(peers)
		if len(peers) > maxProbePeers {
			peers = peers[:maxProbePeers]
		}
		candidates, err := candidatesAfter(network, node, siblings.Items, a)
		if err != nil {
			return nil, err
		}
		checks = append(checks, netprobe.InterfaceCheck{
			Interface: a.Interface, Address: own.Addr().String(), Candidates: candidates, Peers: peers,
		})
	}
	return checks, nil
}

// probeStartTimeout is how long a probe may take to start running. Kubelet retries
// a sandbox that fails to attach, and a first attempt that races Multus or a
// plugin's first use is not a fault, so a probe younger than this is only slow.
const probeStartTimeout = time.Minute

// waitForProbe holds while the probe starts. A probe still pending after the
// timeout is removed and the node is held with the pod's name, whose events say why.
func (r *StorageNodeReconciler) waitForProbe(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, pod *corev1.Pod,
) (nodeStep, bool, error) {
	if pod.Status.Phase != corev1.PodPending || time.Since(pod.CreationTimestamp.Time) < probeStartTimeout {
		return stepCheckingNetwork, false, nil
	}
	if err := r.removeProbe(ctx, pod); err != nil {
		return stepCheckingNetwork, false, err
	}
	return stepCheckingNetwork, false, r.heldFor(cluster, NetworkAttachmentFailed,
		"the network probe pod %s/%s did not start within %s; its events say why, "+
			"for example a missing CNI plugin or a Multus outage",
		pod.Namespace, pod.Name, probeStartTimeout)
}

// readProbe judges a finished guest probe. A probe that fails the node is removed
// at once. One that passes stays while the host probe pings its address, and both
// are removed before the node leaves the step.
func (r *StorageNodeReconciler) readProbe(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster, pod *corev1.Pod, attachments []Attachment,
) (nodeStep, bool, error) {
	if held := r.judgeProbe(pod, cluster, attachments); held != nil {
		var occupied *addressInUseError
		if errors.As(held, &occupied) {
			return stepCheckingNetwork, false, r.turnAwayAddress(ctx, node, cluster, occupied.attachment, occupied.answering)
		}
		networkPreflightSeconds.WithLabelValues(cluster.Name).Observe(time.Since(pod.CreationTimestamp.Time).Seconds())
		if err := r.removeProbes(ctx, node); err != nil {
			return stepCheckingNetwork, false, err
		}
		return stepCheckingNetwork, false, held
	}

	if err := r.rememberAnswering(ctx, node, pod, attachments); err != nil {
		return stepCheckingNetwork, false, err
	}
	host, found, err := r.probePod(ctx, node, netprobe.KindHost)
	if err != nil {
		return stepCheckingNetwork, false, err
	}
	if !found {
		return stepCheckingNetwork, false, r.startHostProbe(ctx, node, attachments)
	}
	if host.DeletionTimestamp != nil {
		return stepCheckingNetwork, false, nil
	}
	if !hostProbeFinished(host) && time.Since(host.CreationTimestamp.Time) < hostProbeTimeout {
		return stepCheckingNetwork, false, nil
	}

	networkPreflightSeconds.WithLabelValues(cluster.Name).Observe(time.Since(pod.CreationTimestamp.Time).Seconds())
	if err := r.recordHostReachability(ctx, node, cluster, host, attachments); err != nil {
		return stepCheckingNetwork, false, err
	}
	if err := r.removeProbes(ctx, node); err != nil {
		return stepCheckingNetwork, false, err
	}
	return stepAwaitingSlot, true, nil
}

// hostProbeTimeout is how long the node waits for the host probe. Its result is a
// warning, so a probe that does not answer in time is treated as having no result
// rather than as a reason to hold the node.
const hostProbeTimeout = 2 * time.Minute

func hostProbeFinished(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// startHostProbe creates the pod that pings the node's addresses from the host
// network namespace of its worker.
func (r *StorageNodeReconciler) startHostProbe(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, attachments []Attachment,
) error {
	targets := make([]string, 0, len(attachments))
	for _, a := range attachments {
		prefix, err := netip.ParsePrefix(a.IPs[0])
		if err != nil {
			return fmt.Errorf("the address %q on network %q: %w", a.IPs[0], a.Network, err)
		}
		targets = append(targets, prefix.Addr().String())
	}
	pod, err := netprobe.HostPod(netprobe.HostOptions{
		Namespace: node.Namespace, Node: node.Name, Worker: node.Spec.WorkerNode,
		Image: r.ProbeImage, Owner: ownerOf(node), Targets: targets,
	})
	if err != nil {
		return fmt.Errorf("building the host probe: %w", err)
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating the host probe: %w", err)
	}
	return nil
}

// recordHostReachability writes what the host probe found to status.network and
// warns for an address the host cannot reach. A probe that did not finish records
// nothing. Neither outcome holds the node.
func (r *StorageNodeReconciler) recordHostReachability(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster, host *corev1.Pod, attachments []Attachment,
) error {
	if !hostProbeFinished(host) {
		return nil
	}
	result, err := netprobe.ParseHost(terminationMessage(host))
	if err != nil {
		// A result that cannot be read is no result, and this probe only warns.
		logf.FromContext(ctx).Info("the host probe left no readable result", "node", node.Name, "error", err.Error())
		return nil
	}

	reachable := map[string]bool{}
	for _, a := range attachments {
		prefix, err := netip.ParsePrefix(a.IPs[0])
		if err != nil {
			continue
		}
		reachable[a.Network] = result.Reached(prefix.Addr().String())
	}
	if err := r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
		status.Network = withHostReachability(status.Network, attachments, reachable)
	}); err != nil {
		return err
	}
	recordNetworkMetrics(cluster.Name, node.Name, withHostReachability(node.Status.Network, attachments, reachable))

	for _, a := range attachments {
		if !reachable[a.Network] {
			recordAttachFailure(cluster.Name, DataNetworkHostUnreachable)
			r.emit(node, corev1.EventTypeWarning, DataNetworkHostUnreachable, fmt.Sprintf(
				"The host of worker %s cannot reach %s on network %q, so volumes cannot be attached there",
				node.Spec.WorkerNode, a.IPs[0], a.Network))
		}
	}
	return nil
}

// withHostReachability sets hostReachable on each network's entry, adding a
// Pending entry for a network the status does not hold yet.
func withHostReachability(
	current []simplyblockv1alpha2.NodeNetworkStatus, attachments []Attachment, reachable map[string]bool,
) []simplyblockv1alpha2.NodeNetworkStatus {
	next := slices.Clone(current)
	for _, a := range attachments {
		value := reachable[a.Network]
		index := slices.IndexFunc(next, func(n simplyblockv1alpha2.NodeNetworkStatus) bool { return n.Network == a.Network })
		if index < 0 {
			next = append(next, simplyblockv1alpha2.NodeNetworkStatus{
				Network: a.Network, Interface: a.Interface, Phase: simplyblockv1alpha2.NodeNetworkPhasePending,
			})
			index = len(next) - 1
		}
		next[index].HostReachable = &value
	}
	return next
}

// judgeProbe is nil for a probe that passed, and otherwise the error that holds
// the node: no attach, then a different address, then a duplicate, then a silent peer.
func (r *StorageNodeReconciler) judgeProbe(
	pod *corev1.Pod, cluster *simplyblockv1alpha2.StorageCluster, attachments []Attachment,
) error {
	message := terminationMessage(pod)
	result, err := netprobe.ParseGuest(message)
	if err != nil {
		return r.heldFor(cluster, NetworkAttachmentFailed, "the network probe left no result: %v", err)
	}

	verdicts, err := VerifyAttachments(attachments, pod.Annotations[NetworkStatusAnnotation], true)
	if err != nil {
		return r.heldFor(cluster, NetworkAttachmentFailed, "%v", err)
	}
	for _, v := range verdicts {
		switch v.Phase {
		case simplyblockv1alpha2.NodeNetworkPhaseMismatched:
			return r.heldFor(cluster, NetworkAddressMismatch,
				"the probe holds %s on network %q, which differs from the declared %s", v.Reported, v.Network, v.Declared)
		case simplyblockv1alpha2.NodeNetworkPhaseAttached:
		default:
			return r.heldFor(cluster, NetworkAttachmentFailed,
				"the probe holds no interface %s for network %q", v.Interface, v.Network)
		}
	}
	for _, a := range attachments {
		answering := result.AnsweringAddresses(a.Interface)
		if slices.ContainsFunc(answering, func(address string) bool { return sameHostAddress(address, a.IPs[0]) }) {
			return &addressInUseError{attachment: a, answering: answering}
		}
	}
	return r.judgePeers(cluster, pod, result, attachments)
}

// addressInUseError is a probe's finding that another host answers for its
// address. It does not hold the node, which takes another address from the pool.
type addressInUseError struct {
	attachment Attachment
	answering  []string
}

func (e *addressInUseError) Error() string {
	return fmt.Sprintf("another host answers for %s on network %q", e.attachment.IPs[0], e.attachment.Network)
}

// judgePeers holds the node for a peer the probe asked for and did not reach.
func (r *StorageNodeReconciler) judgePeers(
	cluster *simplyblockv1alpha2.StorageCluster, pod *corev1.Pod, result netprobe.GuestResult, attachments []Attachment,
) error {
	asked := probePeers(pod)
	for _, a := range attachments {
		for _, peer := range asked[a.Interface] {
			if !result.PeerReached(a.Interface, peer) {
				return r.heldFor(cluster, DataNetworkUnreachable,
					"the peer %s on network %q does not answer from %s", peer, a.Network, a.Interface)
			}
		}
	}
	return nil
}

// probePeers is the peers the probe was told to ping, by interface, read back
// from its environment so the verdict judges what was asked.
func probePeers(pod *corev1.Pod) map[string][]string {
	peers := map[string][]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name != "PEERS" || e.Value == "" {
			continue
		}
		for _, pair := range strings.Split(e.Value, ",") {
			if name, address, ok := strings.Cut(pair, "="); ok {
				peers[name] = append(peers[name], address)
			}
		}
	}
	return peers
}

// terminationMessage is what the probe's container wrote to its termination log.
func terminationMessage(pod *corev1.Pod) string {
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Terminated != nil {
			return status.State.Terminated.Message
		}
	}
	return ""
}

// removeProbe deletes one probe pod, and treats one already gone as deleted.
func (r *StorageNodeReconciler) removeProbe(ctx context.Context, pod *corev1.Pod) error {
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting the network probe: %w", err)
	}
	return nil
}

// removeProbes deletes both of the node's probes.
func (r *StorageNodeReconciler) removeProbes(ctx context.Context, node *simplyblockv1alpha2.StorageNode) error {
	for _, kind := range []string{netprobe.KindGuest, netprobe.KindHost} {
		pod, found, err := r.probePod(ctx, node, kind)
		if err != nil {
			return err
		}
		if found {
			if err := r.removeProbe(ctx, pod); err != nil {
				return err
			}
		}
	}
	return nil
}

// probeGone is what Posting asks before it makes the node-add call: both probes
// are removed, and the call waits until they are gone, so a probe and the SPDK
// pod never hold an address together.
func (r *StorageNodeReconciler) probeGone(ctx context.Context, node *simplyblockv1alpha2.StorageNode) (bool, error) {
	_, guest, err := r.probePod(ctx, node, netprobe.KindGuest)
	if err != nil {
		return false, err
	}
	_, host, err := r.probePod(ctx, node, netprobe.KindHost)
	if err != nil {
		return false, err
	}
	if !guest && !host {
		return true, nil
	}
	return false, r.removeProbes(ctx, node)
}

// ownerOf is the owner reference that makes the probe go with its node.
func ownerOf(node *simplyblockv1alpha2.StorageNode) metav1.OwnerReference {
	controller, block := true, true
	return metav1.OwnerReference{
		APIVersion: simplyblockv1alpha2.GroupVersion.String(), Kind: "StorageNode",
		Name: node.Name, UID: node.UID, Controller: &controller, BlockOwnerDeletion: &block,
	}
}
