// The per-pod DNS names the control plane drives an SPDK process by.
//
// A storage node's SPDK process answers RPC on its own port, and the control
// plane reaches it at
// <worker>.simplyblock-spdk-proxy.<namespace>.svc.cluster.local:<rpc port>.
// That name is a headless Service plus one EndpointSlice per port, each endpoint
// carrying the worker's name as its hostname -- the Service on its own resolves
// nothing.
//
// It is only that name under TLS. simplyblock_core/models/storage_node.py dials
// mgmt_ip directly while tls_connect is "disabled", so a plaintext deployment
// never asks DNS for any of this and a deployment that serves TLS cannot add a
// single node without it. The reconcile that published these slices belonged to
// StorageNodeSet and went out with that kind; nothing used the names until TLS
// became the default, and then every node add stalled in the control plane's
// resolver retry loop.
//
// What the two passes below are careful about is the difference between a pod
// that is gone and a pod that is merely not ready this instant. Deleting a
// slice for the second costs the node its address mid-operation, which is the
// shape of an earlier outage: readiness flips on a single missed probe tick,
// well short of anything that restarts a container.

package node

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// spdkProxyServiceName is the headless Service these slices attach to, and it is
// a literal because the control plane's own address template is.
const spdkProxyServiceName = "simplyblock-spdk-proxy"

// reconcileSpdkProxyEndpoints publishes one EndpointSlice per RPC port.
func (r *StorageNodeWorkloadReconciler) reconcileSpdkProxyEndpoints(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	log := logf.FromContext(ctx)

	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{"role": utils.LabelSpdkProxyRole},
	); err != nil {
		return fmt.Errorf("list the spdk-proxy pods: %w", err)
	}

	// Two sets, and the difference between them is the whole of the delete pass
	// below. byPort holds the pods that can serve an address now; anyPod holds
	// every port that has a pod object at all, ready or not. The RPC port is
	// readable from the pod spec the moment it is scheduled, long before it is
	// ready, so the second is safe to compute from the full list.
	byPort := map[int32][]utils.SpdkProxyEndpoint{}
	anyPod := map[int32]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		rpcPort, ok := spdkProxyRPCPort(pod)
		if !ok {
			log.Info("an spdk-proxy pod names no RPC port and is not published",
				"pod", pod.Name)
			continue
		}
		anyPod[rpcPort] = true
		if !spdkProxyPodServesAnAddress(pod) {
			continue
		}
		byPort[rpcPort] = append(byPort[rpcPort], utils.SpdkProxyEndpoint{
			NodeName: pod.Spec.NodeName,
			PodIP:    pod.Status.PodIP,
			RpcPort:  rpcPort,
		})
	}

	for rpcPort, endpoints := range byPort {
		if err := r.applyProxySlice(ctx, cluster, rpcPort, endpoints); err != nil {
			return err
		}
	}
	return r.pruneProxySlices(ctx, cluster, anyPod)
}

// applyProxySlice writes one port's slice.
func (r *StorageNodeWorkloadReconciler) applyProxySlice(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	rpcPort int32,
	endpoints []utils.SpdkProxyEndpoint,
) error {
	desired, err := utils.BuildSpdkProxyEndpointSlice(cluster, rpcPort, endpoints)
	if err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return fmt.Errorf("own the spdk-proxy EndpointSlice for port %d: %w", rpcPort, err)
	}

	var existing discoveryv1.EndpointSlice
	err = r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	return r.Update(ctx, desired)
}

// pruneProxySlices removes the slices of ports that have no pod at all.
//
// The test is against every port with a pod object rather than every port with a
// ready one. A pod that is not ready for one probe tick keeps its last known
// address until the pass that finds it ready refreshes it; taking the name away
// instead is what left the control plane resolving nothing mid-operation.
func (r *StorageNodeWorkloadReconciler) pruneProxySlices(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	anyPod map[int32]bool,
) error {
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{"kubernetes.io/service-name": spdkProxyServiceName},
	); err != nil {
		return fmt.Errorf("list the published spdk-proxy EndpointSlices: %w", err)
	}

	for i := range slices.Items {
		slice := &slices.Items[i]
		if !metav1.IsControlledBy(slice, cluster) {
			continue
		}
		serving := false
		for _, port := range slice.Ports {
			if port.Port != nil && anyPod[*port.Port] {
				serving = true
				break
			}
		}
		if serving {
			continue
		}
		if err := r.Delete(ctx, slice); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete the stale spdk-proxy EndpointSlice %s: %w", slice.Name, err)
		}
	}
	return nil
}

// spdkProxyPodServesAnAddress reports whether a pod can be published.
//
// It is not the same question as whether the pod is healthy. What an endpoint
// needs is a worker and an address, and every container reporting ready is what
// says the proxy is listening on it.
func spdkProxyPodServesAnAddress(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	if pod.Spec.NodeName == "" || pod.Status.PodIP == "" {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			return false
		}
	}
	return len(pod.Status.ContainerStatuses) > 0
}

// spdkProxyRPCPort is the port a pod's proxy answers on.
//
// RPC_PORT on the proxy container is the statement of it. The pod name is a
// fallback because it carries the same number, and a pod the control plane
// created with an older template is still a pod this has to publish.
func spdkProxyRPCPort(pod *corev1.Pod) (int32, bool) {
	for _, container := range pod.Spec.Containers {
		if container.Name != "spdk-proxy-container" {
			continue
		}
		for _, env := range container.Env {
			if env.Name != "RPC_PORT" || env.Value == "" {
				continue
			}
			port, err := strconv.ParseInt(env.Value, 10, 32)
			if err != nil {
				return 0, false
			}
			return int32(port), true
		}
	}

	rest, ok := strings.CutPrefix(pod.Name, "snode-spdk-pod-")
	if !ok {
		return 0, false
	}
	dash := strings.Index(rest, "-")
	if dash <= 0 {
		return 0, false
	}
	port, err := strconv.ParseInt(rest[:dash], 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(port), true
}
