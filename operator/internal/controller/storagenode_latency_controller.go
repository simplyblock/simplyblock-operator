/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/autoplacement"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	defaultLatencyBenchmarkInterval = 5 * time.Minute

	baselineJobLabelKey           = "simplyblock.io/fio-baseline"
	baselineJobNodeLabelKey       = "simplyblock.io/fio-baseline-node"
	baselineJobTTLSeconds   int32 = 3600
	baselineJobNamePrefix         = "sb-fio-baseline-"
)

// The JSON wire types shared with the simplyblock-rebalancer binary
// (rebalancer.NodeConfig for the ConfigMap, rebalancer.LatencyResult for the baseline
// termination log) live in internal/rebalancer so both sides share one definition.

// StorageNodeLatencyReconciler measures per-node NVMe-oF write latency using a
// one-shot Kubernetes Job for the initial empty-cluster baseline. Ongoing runtime
// measurements are pushed directly to Prometheus by the simplyblock-rebalancer sidecar.
type StorageNodeLatencyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Provisioner manages the benchmark storage pool and per-node volumes.
	// Defaults to AutomaticBenchmarkProvisioner (no-op) when nil, which assumes the pool
	// and volumes are created automatically during cluster setup.
	// Set to WebAPIBenchmarkProvisioner for test environments that require explicit provisioning.
	Provisioner BenchmarkProvisioner

	// APIClient queries the simplyblock REST API to resolve a storage node's
	// data-network IP from its NIC listing. Independent of the provisioner.
	APIClient *webapi.Client
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodesets,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;delete;get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=create;get;update;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *StorageNodeLatencyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	snode := &simplyblockv1alpha2.StorageCluster{}
	if err := r.Get(ctx, req.NamespacedName, snode); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterCR := &simplyblockv1alpha2.StorageCluster{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: req.Namespace,
		Name:      snode.Name,
	}, clusterCR); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	spec := autoplacement.GetConfig(clusterCR.Spec.VolumeAutoPlacement)
	if !ptr.BoolFromOrFalse(spec.EnableLatencyBenchmark) {
		return ctrl.Result{}, nil
	}
	// The latency/baseline Jobs reuse the existing top-level rebalancer image
	// (VolumeMigrationSettings.RebalancerImage); there is no separate image.
	vms := volumemigration.GetConfig(clusterCR.Spec.VolumeMigrationSettings)
	rebalancerImage := ptr.From(vms.RebalancerImage, "")
	if rebalancerImage == "" {
		log.Info("RebalancerImage not configured; latency benchmark disabled")
		return ctrl.Result{}, nil
	}

	benchInterval := defaultLatencyBenchmarkInterval
	if d := ptr.From(spec.LatencyBenchmarkInterval, metav1.Duration{}); d.Duration > 0 {
		benchInterval = d.Duration
	}

	if clusterCR.Status.UUID == "" || clusterCR.Status.NQN == "" {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if clusterCR.Status.Status != utils.ClusterStatusActive {
		// Expected, transient condition during cluster startup: the latency controller
		// reconciles on every StorageNode status write, so logging this at INFO floods
		// the operator log with thousands of identical lines while the cluster activates.
		// Keep it at debug verbosity.
		log.V(1).Info("Cluster not yet active, deferring benchmark volume provisioning",
			"clusterStatus", clusterCR.Status.Status)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	poolUUID, err := r.Provisioner.EnsurePool(ctx, snode.Namespace, snode.Name)
	if err != nil {
		log.Error(err, "Cannot ensure benchmark pool")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// One baseline Job per backend node. On NUMA hosts several of them share a
	// Kubernetes hostname while having independent NVMe devices and independent
	// latency characteristics, so each is measured separately — which is what makes
	// the StorageNode object, rather than the host, the thing a reading belongs to.
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(snode.Namespace)); err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// hostConfigs accumulates per-node configs keyed by Kubernetes hostname so the
	// sidecar, which is one pod per host, receives a JSON array covering every NUMA
	// node on its host.
	hostConfigs := map[string][]autoplacement.NodeConfig{}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != snode.Name || node.Status.UUID == "" ||
			node.Status.Status != utils.NodeStatusOnline || !node.Status.Health ||
			node.Status.Hostname == "" {
			continue
		}
		r.processNodeBaseline(ctx, snode, clusterCR, poolUUID, node, rebalancerImage, hostConfigs)
	}

	configData := make(map[string]string, len(hostConfigs))
	for hostname, cfgs := range hostConfigs {
		raw, _ := json.Marshal(cfgs)
		configData[hostname] = string(raw)
	}
	if err := r.reconcileConfigMap(ctx, snode.Namespace, snode.Name, configData); err != nil {
		log.Error(err, "Cannot reconcile simplyblock-rebalancer ConfigMap")
	}

	return ctrl.Result{RequeueAfter: benchInterval}, nil
}

// processNodeBaseline ensures the benchmark volume exists, drives the one-shot baseline Job,
// and populates hostConfigs once the baseline has been recorded.
// Returns true when latencyMetrics was changed and needs to be patched.
func (r *StorageNodeLatencyReconciler) processNodeBaseline(
	ctx context.Context,
	snode *simplyblockv1alpha2.StorageCluster,
	clusterCR *simplyblockv1alpha2.StorageCluster,
	poolUUID string,
	node *simplyblockv1alpha2.StorageNode,
	image string,
	hostConfigs map[string][]autoplacement.NodeConfig,
) {
	log := logf.FromContext(ctx)

	nodeUUID := node.Status.UUID
	measured := node.Status.LatencyMetrics

	volumeUUID, err := r.Provisioner.EnsureVolume(
		ctx, snode.Namespace, snode.Name, poolUUID,
		"simplyblock-rebalancer-"+nodeUUID, nodeUUID,
	)
	if err != nil {
		log.Error(err, "Cannot ensure benchmark volume", "node", nodeUUID)
		return
	}

	conn := benchmarkConnInfo{
		NQN:  r.Provisioner.BenchmarkNQN(clusterCR.Status.NQN, volumeUUID),
		Addr: managementAddress(node),
		Port: logicalVolumeConnectionPort(node),
	}
	// The lvol's NVMe-oF subsystem listens on the node's data NIC, not its management
	// IP, so targeting the management address fails with a refused connection.
	// Resolve the node's data-network address from its NIC listing, and fall back to
	// the management address
	// only when it cannot be resolved.
	if dataAddr, err := r.nodeDataAddr(ctx, clusterCR.Status.UUID, nodeUUID); err != nil {
		log.Info("Could not resolve data-network address; falling back to management IP",
			"node", nodeUUID, "addr", conn.Addr, "error", err.Error())
	} else {
		conn.Addr = dataAddr
	}

	if measured == nil || measured.BaselineP99NS == 0 {
		baseline, _, err := r.reconcileBaselineJob(ctx, snode, node, conn, image)
		if err != nil {
			log.Error(err, "Baseline job error", "node", nodeUUID)
		}
		if baseline != nil {
			now := metav1.NewTime(time.Now())
			measured = &simplyblockv1alpha2.NodeLatencyMetrics{
				NodeUUID:           nodeUUID,
				BaselineP50NS:      baseline.P50NS,
				BaselineP99NS:      baseline.P99NS,
				BaselineMeasuredAt: &now,
			}
			log.Info("Baseline measured", "node", nodeUUID,
				"p50ns", baseline.P50NS, "p99ns", baseline.P99NS)
			if err := r.patchLatencyStatus(ctx, node, measured); err != nil {
				log.Error(err, "Failed to record the node's baseline", "node", nodeUUID)
			}
		}
	}

	// Only expose the ConfigMap entry to the sidecar once the baseline is stored.
	// This prevents the sidecar's continuous fio loop from running concurrently
	// with the one-shot baseline Job — both would write to the same NVMe device
	// and corrupt each other's measurements.
	if measured != nil && measured.BaselineP99NS > 0 {
		host := node.Status.Hostname
		hostConfigs[host] = append(hostConfigs[host], autoplacement.NodeConfig{
			NQN:         conn.NQN,
			Addr:        conn.Addr,
			Port:        conn.Port,
			NodeUUID:    nodeUUID,
			ClusterUUID: clusterCR.Status.UUID,
		})
	}
}

// benchmarkConnInfo holds the NVMe-oF connection parameters for the benchmark volume.
// These are derived at runtime from the cluster NQN and the node's reported address/port;
// no API call is needed because the benchmark volume exists automatically on every node.
type benchmarkConnInfo struct {
	NQN  string
	Addr string
	Port int32
}

// reconcileBaselineJob manages the lifecycle of the one-shot baseline measurement Job
// for a single backend node. Returns the parsed result once the Job succeeds.
func (r *StorageNodeLatencyReconciler) reconcileBaselineJob(
	ctx context.Context,
	snode *simplyblockv1alpha2.StorageCluster,
	node *simplyblockv1alpha2.StorageNode,
	conn benchmarkConnInfo,
	image string,
) (*autoplacement.LatencyResult, bool, error) {
	jobName := baselineJobNamePrefix + volumemigration.JobNameID(node.Status.UUID)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: snode.Namespace, Name: jobName}, job)

	if err != nil && !apierrors.IsNotFound(err) {
		return nil, false, fmt.Errorf("get baseline job: %w", err)
	}

	if err == nil {
		if r.jobSucceeded(job) {
			// Do not delete the Job here — leave it for TTLSecondsAfterFinished to reap.
			// Deleting inline turns a cache-lagged reconcile into a re-measurement: a stale
			// reconcile (whose snapshot predates the persisted baseline) would see the Job
			// gone, treat the node as unmeasured, and create a fresh benchmark Job. Leaving
			// the succeeded Job in place means such a reconcile re-reads the same idempotent
			// result instead. Once the baseline is persisted, the BaselineP99NS>0 guard
			// short-circuits before this function is ever called again.
			result, readErr := r.readJobResult(ctx, job)
			if readErr != nil {
				return nil, true, readErr
			}
			return result, true, nil
		}
		if r.jobFailed(job) {
			_ = r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
			return nil, true, nil // recreated on next reconcile
		}
		return nil, false, nil // still running
	}

	// No job yet — create one if all connection info is available.
	if node.Status.Hostname == "" || conn.Addr == "" || conn.NQN == "" {
		return nil, false, nil
	}
	if createErr := r.createBaselineJob(ctx, snode, node, conn, image); createErr != nil {
		return nil, false, fmt.Errorf("create baseline job: %w", createErr)
	}
	return nil, true, nil
}

// storageNodeTolerations is what the cluster's storage-node pods tolerate,
// which is what anything the operator pins to one of those machines tolerates
// as well.
func storageNodeTolerations(cluster *simplyblockv1alpha2.StorageCluster) []corev1.Toleration {
	if cluster == nil || cluster.Spec.StorageNodes == nil {
		return nil
	}
	return cluster.Spec.StorageNodes.Tolerations
}

func (r *StorageNodeLatencyReconciler) createBaselineJob(
	ctx context.Context,
	snode *simplyblockv1alpha2.StorageCluster,
	node *simplyblockv1alpha2.StorageNode,
	conn benchmarkConnInfo,
	image string,
) error {
	privileged := true
	ttl := baselineJobTTLSeconds
	backoffLimit := int32(2)
	hostDevPath := "/dev"

	return r.Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      baselineJobNamePrefix + volumemigration.JobNameID(node.Status.UUID),
			Namespace: snode.Namespace,
			Labels: map[string]string{
				baselineJobLabelKey:     "true",
				baselineJobNodeLabelKey: node.Status.UUID,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(snode, simplyblockv1alpha2.GroupVersion.WithKind("StorageCluster")),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  map[string]string{"kubernetes.io/hostname": node.Status.Hostname},
					// The Job runs on the storage node it measures, so it has
					// to tolerate what that node tolerates: pinning asks the
					// scheduler for the machine rather than excusing the pod
					// from its taints, and a fleet with a dedicated storage
					// plane taints every machine this can run on.
					Tolerations: storageNodeTolerations(snode),
					HostNetwork: true,
					Volumes: []corev1.Volume{
						{
							Name: "host-dev",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: hostDevPath},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "simplyblock-rebalancer-baseline",
							Image:           image,
							ImagePullPolicy: corev1.PullAlways,
							Command: []string{
								"simplyblock-rebalancer",
								"--mode=baseline",
								"--addr=$(FIO_NODE_ADDR)",
								"--port=$(FIO_NODE_PORT)",
								"--nqn=$(FIO_VOLUME_NQN)",
								"--termination-log=/tmp/termination-log",
							},
							TerminationMessagePath: "/tmp/termination-log",
							SecurityContext:        &corev1.SecurityContext{Privileged: &privileged},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "host-dev", MountPath: "/dev"},
							},
							Env: []corev1.EnvVar{
								{Name: "FIO_NODE_ADDR", Value: conn.Addr},
								{Name: "FIO_NODE_PORT", Value: fmt.Sprintf("%d", conn.Port)},
								{Name: "FIO_VOLUME_NQN", Value: conn.NQN},
							},
						},
					},
				},
			},
		},
	})
}

// nodeDataAddr resolves a storage node's data-network IP from its NIC listing,
// returning the first interface that is UP with a non-empty address. The lvol
// subsystem listens on the data NIC, so the fio baseline must target this address
// rather than the node's management IP. Returns an error when no API client is
// configured or no usable data NIC is reported.
func (r *StorageNodeLatencyReconciler) nodeDataAddr(ctx context.Context, clusterUUID, nodeUUID string) (string, error) {
	if r.APIClient == nil {
		return "", fmt.Errorf("no API client configured")
	}
	nics, err := r.APIClient.GetStorageNodeNICs(ctx, clusterUUID, nodeUUID)
	if err != nil {
		return "", err
	}
	for _, nic := range nics {
		if nic.Address != "" && strings.EqualFold(nic.Status, "UP") {
			return nic.Address, nil
		}
	}
	return "", fmt.Errorf("no UP data NIC found for node %s", nodeUUID)
}

// logicalVolumeConnectionPort returns the NVMe/TCP connection port for a node, falling back to 4430 if not reported.
func logicalVolumeConnectionPort(node *simplyblockv1alpha2.StorageNode) int32 {
	if node.Status.Ports != nil && node.Status.Ports.Lvol != nil && *node.Status.Ports.Lvol > 0 {
		return *node.Status.Ports.Lvol
	}
	return 4430
}

// readJobResult reads the fio result from the baseline container's termination message.
func (r *StorageNodeLatencyReconciler) readJobResult(ctx context.Context, job *batchv1.Job) (*autoplacement.LatencyResult, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name},
	); err != nil {
		return nil, fmt.Errorf("list job pods: %w", err)
	}
	if len(podList.Items) == 0 {
		return nil, fmt.Errorf("no pods found for job %s", job.Name)
	}

	for _, cs := range podList.Items[0].Status.ContainerStatuses {
		if cs.Name != "simplyblock-rebalancer-baseline" || cs.State.Terminated == nil {
			continue
		}
		var result autoplacement.LatencyResult
		if err := json.Unmarshal([]byte(cs.State.Terminated.Message), &result); err != nil {
			return nil, fmt.Errorf("parse termination message: %w", err)
		}
		return &result, nil
	}
	return nil, fmt.Errorf("no termination message for job %s", job.Name)
}

// reconcileConfigMap creates or updates the per-cluster ConfigMap that maps Kubernetes
// node hostname → benchmark volume config JSON consumed by the simplyblock-rebalancer sidecar.
func (r *StorageNodeLatencyReconciler) reconcileConfigMap(
	ctx context.Context,
	namespace, clusterName string,
	data map[string]string,
) error {
	name := utils.SimplyblockRebalancerConfigMapName(clusterName)
	var existing corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data:       data,
		})
	}
	if err != nil {
		return err
	}
	existing.Data = data
	return r.Update(ctx, &existing)
}

func (r *StorageNodeLatencyReconciler) jobSucceeded(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *StorageNodeLatencyReconciler) jobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// patchLatencyStatus records one node's baseline on the node itself.
//
// The reading moved off the retired fleet object for the reason the reading is one
// node's: a fleet-wide list made every node's measurement a write to one object
// shared by all of them, and a stale snapshot of that list silently dropped
// entries a concurrent reconcile had written. One node, one write, and no list to
// lose an entry from.
func (r *StorageNodeLatencyReconciler) patchLatencyStatus(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	measured *simplyblockv1alpha2.NodeLatencyMetrics,
) error {
	patch := client.MergeFrom(node.DeepCopy())
	node.Status.LatencyMetrics = measured
	return r.Status().Patch(ctx, node, patch)
}

// managementAddress is the node's reported management IP, which is the fallback a
// benchmark connects on when the data NIC cannot be resolved.
func managementAddress(node *simplyblockv1alpha2.StorageNode) string {
	if node.Status.Ports == nil {
		return ""
	}
	return node.Status.Ports.Management
}

func (r *StorageNodeLatencyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Provisioner == nil {
		r.Provisioner = &AutomaticBenchmarkProvisioner{}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageCluster{}).
		Owns(&batchv1.Job{}).
		Named("storagenodelatency").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
