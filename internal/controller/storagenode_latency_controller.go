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

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	defaultLatencyBenchmarkInterval = 5 * time.Minute

	baselineJobLabelKey           = "simplyblock.io/fio-baseline"
	baselineJobNodeLabelKey       = "simplyblock.io/fio-baseline-node"
	baselineJobTTLSeconds   int32 = 3600
	baselineJobNamePrefix         = "sb-fio-baseline-"
)

// fioBenchNodeConfig is one element of the JSON array stored per k8s hostname in the
// fio-bench ConfigMap. The sidecar iterates the array to benchmark every NUMA node on
// its host independently.
type fioBenchNodeConfig struct {
	NQN         string `json:"nqn"`
	Addr        string `json:"addr"`
	Port        int32  `json:"port"`
	NodeUUID    string `json:"nodeUUID"`
	ClusterUUID string `json:"clusterUUID"`
}

// fioBenchResult is the JSON written to the container termination log by the baseline Job.
type fioBenchResult struct {
	P50NS int64 `json:"p50_ns"`
	P99NS int64 `json:"p99_ns"`
}

// StorageNodeLatencyReconciler measures per-node NVMe-oF write latency using a
// one-shot Kubernetes Job for the initial empty-cluster baseline. Ongoing runtime
// measurements are pushed directly to Prometheus by the fio-bench-probe sidecar.
type StorageNodeLatencyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Provisioner manages the benchmark storage pool and per-node volumes.
	// Defaults to AutomaticBenchmarkProvisioner (no-op) when nil, which assumes the pool
	// and volumes are created automatically during cluster setup.
	// Set to WebAPIBenchmarkProvisioner for test environments that require explicit provisioning.
	Provisioner BenchmarkProvisioner
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;delete;get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=create;get;update;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *StorageNodeLatencyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	snode := &simplyblockv1alpha1.StorageNode{}
	if err := r.Get(ctx, req.NamespacedName, snode); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterCR := &simplyblockv1alpha1.StorageCluster{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: req.Namespace,
		Name:      snode.Spec.ClusterName,
	}, clusterCR); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	spec := clusterCR.Spec.VolumeRebalancing
	if spec == nil || spec.LatencyBenchmarkEnabled == nil || !*spec.LatencyBenchmarkEnabled {
		return ctrl.Result{}, nil
	}
	if spec.FioBenchmarkImage == nil || *spec.FioBenchmarkImage == "" {
		log.Info("FioBenchmarkImage not configured; latency benchmark disabled")
		return ctrl.Result{}, nil
	}

	benchInterval := defaultLatencyBenchmarkInterval
	if spec.LatencyBenchmarkInterval != nil && spec.LatencyBenchmarkInterval.Duration > 0 {
		benchInterval = spec.LatencyBenchmarkInterval.Duration
	}

	if clusterCR.Status.UUID == "" || clusterCR.Status.NQN == "" {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if clusterCR.Status.Status != utils.ClusterStatusActive {
		log.Info("Cluster not yet active, deferring benchmark volume provisioning",
			"clusterStatus", clusterCR.Status.Status)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	poolUUID, err := r.Provisioner.EnsurePool(ctx, snode.Namespace, snode.Spec.ClusterName)
	if err != nil {
		log.Error(err, "Cannot ensure benchmark pool")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// One baseline Job per node UUID. On NUMA hosts multiple backend nodes share the
	// same k8s hostname but have independent NVMe devices and independent latency
	// characteristics, so every node UUID is measured separately.
	nodesByUUID := map[string]simplyblockv1alpha1.NodeStatus{}
	for _, n := range snode.Status.Nodes {
		if n.UUID == "" || n.Status != nodeStatusOnline || !n.Health || n.Hostname == "" {
			continue
		}
		if _, seen := nodesByUUID[n.UUID]; !seen {
			nodesByUUID[n.UUID] = n
		}
	}

	latencyMetrics := r.copyLatencyMetrics(snode.Status.LatencyMetrics)
	// hostConfigs accumulates per-node configs keyed by k8s hostname so the sidecar
	// (one pod per host) receives a JSON array covering all NUMA nodes on its host.
	hostConfigs := map[string][]fioBenchNodeConfig{}
	changed := false

	for _, node := range nodesByUUID {
		nodeChanged := r.processNodeBaseline(ctx, snode, clusterCR, poolUUID, node, *spec.FioBenchmarkImage, &latencyMetrics, hostConfigs)
		if nodeChanged {
			changed = true
		}
	}

	configData := make(map[string]string, len(hostConfigs))
	for hostname, cfgs := range hostConfigs {
		raw, _ := json.Marshal(cfgs)
		configData[hostname] = string(raw)
	}
	if err := r.reconcileConfigMap(ctx, snode.Namespace, snode.Spec.ClusterName, configData); err != nil {
		log.Error(err, "Cannot reconcile fio-bench ConfigMap")
	}

	if changed {
		if err := r.patchLatencyStatus(ctx, snode, latencyMetrics); err != nil {
			log.Error(err, "Failed to patch StorageNode latency status")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	return ctrl.Result{RequeueAfter: benchInterval}, nil
}

// processNodeBaseline ensures the benchmark volume exists, drives the one-shot baseline Job,
// and populates hostConfigs once the baseline has been recorded.
// Returns true when latencyMetrics was changed and needs to be patched.
func (r *StorageNodeLatencyReconciler) processNodeBaseline(
	ctx context.Context,
	snode *simplyblockv1alpha1.StorageNode,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	poolUUID string,
	node simplyblockv1alpha1.NodeStatus,
	image string,
	latencyMetrics *[]simplyblockv1alpha1.NodeLatencyMetrics,
	hostConfigs map[string][]fioBenchNodeConfig,
) bool {
	log := logf.FromContext(ctx)

	m := r.findOrCreateEntry(*latencyMetrics, node.UUID)

	volumeUUID, err := r.Provisioner.EnsureVolume(
		ctx, snode.Namespace, snode.Spec.ClusterName, poolUUID,
		"fio-bench-"+node.UUID, node.UUID,
	)
	if err != nil {
		log.Error(err, "Cannot ensure benchmark volume", "node", node.UUID)
		return false
	}

	conn := benchmarkConnInfo{
		NQN:  r.Provisioner.BenchmarkNQN(clusterCR.Status.NQN, volumeUUID),
		Addr: node.MgmtIp,
		Port: logicalVolumeConnectionPort(node),
	}

	changed := false
	if m.BaselineP99NS == 0 {
		baseline, jobChanged, err := r.reconcileBaselineJob(ctx, snode, node, conn, image)
		if err != nil {
			log.Error(err, "Baseline job error", "node", node.UUID)
		}
		if baseline != nil {
			now := metav1.NewTime(time.Now())
			m.BaselineP50NS = baseline.P50NS
			m.BaselineP99NS = baseline.P99NS
			m.BaselineMeasuredAt = &now
			log.Info("Baseline measured", "node", node.UUID, "p99ns", baseline.P99NS)
			jobChanged = true
		}
		if jobChanged {
			changed = true
		}
	}

	// Only expose the ConfigMap entry to the sidecar once the baseline is stored.
	// This prevents the sidecar's continuous fio loop from running concurrently
	// with the one-shot baseline Job — both would write to the same NVMe device
	// and corrupt each other's measurements.
	if m.BaselineP99NS > 0 {
		hostConfigs[node.Hostname] = append(hostConfigs[node.Hostname], fioBenchNodeConfig{
			NQN:         conn.NQN,
			Addr:        conn.Addr,
			Port:        conn.Port,
			NodeUUID:    node.UUID,
			ClusterUUID: clusterCR.Status.UUID,
		})
	}

	*latencyMetrics = r.setEntry(*latencyMetrics, m)
	return changed
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
	snode *simplyblockv1alpha1.StorageNode,
	node simplyblockv1alpha1.NodeStatus,
	conn benchmarkConnInfo,
	image string,
) (*fioBenchResult, bool, error) {
	jobName := baselineJobNamePrefix + safeNodeID(node.UUID)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: snode.Namespace, Name: jobName}, job)

	if err != nil && !apierrors.IsNotFound(err) {
		return nil, false, fmt.Errorf("get baseline job: %w", err)
	}

	if err == nil {
		if r.jobSucceeded(job) {
			result, readErr := r.readJobResult(ctx, job)
			_ = r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
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
	if node.Hostname == "" || conn.Addr == "" || conn.NQN == "" {
		return nil, false, nil
	}
	if createErr := r.createBaselineJob(ctx, snode, node, conn, image); createErr != nil {
		return nil, false, fmt.Errorf("create baseline job: %w", createErr)
	}
	return nil, true, nil
}

func (r *StorageNodeLatencyReconciler) createBaselineJob(
	ctx context.Context,
	snode *simplyblockv1alpha1.StorageNode,
	node simplyblockv1alpha1.NodeStatus,
	conn benchmarkConnInfo,
	image string,
) error {
	privileged := true
	ttl := baselineJobTTLSeconds
	backoffLimit := int32(2)
	hostDevPath := "/dev"

	return r.Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      baselineJobNamePrefix + safeNodeID(node.UUID),
			Namespace: snode.Namespace,
			Labels: map[string]string{
				baselineJobLabelKey:     "true",
				baselineJobNodeLabelKey: node.UUID,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(snode, simplyblockv1alpha1.GroupVersion.WithKind("StorageNode")),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  map[string]string{"kubernetes.io/hostname": node.Hostname},
					HostNetwork:   true,
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
							Name:            "fio-baseline",
							Image:           image,
							ImagePullPolicy: corev1.PullAlways,
							Command: []string{
								"fio-probe",
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

// logicalVolumeConnectionPort returns the NVMe/TCP connection port for a node, falling back to 4430 if not reported.
func logicalVolumeConnectionPort(node simplyblockv1alpha1.NodeStatus) int32 {
	if node.LvolPort != nil && *node.LvolPort > 0 {
		return *node.LvolPort
	}
	return 4430
}

// readJobResult reads the fio result from the baseline container's termination message.
func (r *StorageNodeLatencyReconciler) readJobResult(ctx context.Context, job *batchv1.Job) (*fioBenchResult, error) {
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
		if cs.Name != "fio-baseline" || cs.State.Terminated == nil {
			continue
		}
		var result fioBenchResult
		if err := json.Unmarshal([]byte(cs.State.Terminated.Message), &result); err != nil {
			return nil, fmt.Errorf("parse termination message: %w", err)
		}
		return &result, nil
	}
	return nil, fmt.Errorf("no termination message for job %s", job.Name)
}

// reconcileConfigMap creates or updates the per-cluster ConfigMap that maps k8s
// node hostname → benchmark volume config JSON consumed by the fio-bench-probe sidecar.
func (r *StorageNodeLatencyReconciler) reconcileConfigMap(
	ctx context.Context,
	namespace, clusterName string,
	data map[string]string,
) error {
	name := utils.FioBenchConfigMapName(clusterName)
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

// safeNodeID produces a DNS-label-safe suffix from a node UUID.
func safeNodeID(nodeUUID string) string {
	s := strings.ReplaceAll(nodeUUID, "-", "")
	if len(s) > 20 {
		s = s[:20]
	}
	return s
}

func (r *StorageNodeLatencyReconciler) patchLatencyStatus(
	ctx context.Context,
	snode *simplyblockv1alpha1.StorageNode,
	latencyMetrics []simplyblockv1alpha1.NodeLatencyMetrics,
) error {
	orig := snode.DeepCopy()
	snode.Status.LatencyMetrics = latencyMetrics
	return r.Status().Patch(ctx, snode, client.MergeFrom(orig))
}

func (r *StorageNodeLatencyReconciler) copyLatencyMetrics(
	src []simplyblockv1alpha1.NodeLatencyMetrics,
) []simplyblockv1alpha1.NodeLatencyMetrics {
	out := make([]simplyblockv1alpha1.NodeLatencyMetrics, len(src))
	copy(out, src)
	return out
}

func (r *StorageNodeLatencyReconciler) findOrCreateEntry(
	metrics []simplyblockv1alpha1.NodeLatencyMetrics,
	nodeUUID string,
) *simplyblockv1alpha1.NodeLatencyMetrics {
	for i := range metrics {
		if metrics[i].NodeUUID == nodeUUID {
			cp := metrics[i]
			return &cp
		}
	}
	return &simplyblockv1alpha1.NodeLatencyMetrics{NodeUUID: nodeUUID}
}

func (r *StorageNodeLatencyReconciler) setEntry(
	metrics []simplyblockv1alpha1.NodeLatencyMetrics,
	m *simplyblockv1alpha1.NodeLatencyMetrics,
) []simplyblockv1alpha1.NodeLatencyMetrics {
	for i := range metrics {
		if metrics[i].NodeUUID == m.NodeUUID {
			metrics[i] = *m
			return metrics
		}
	}
	return append(metrics, *m)
}

func (r *StorageNodeLatencyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Provisioner == nil {
		r.Provisioner = &AutomaticBenchmarkProvisioner{}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageNode{}).
		Owns(&batchv1.Job{}).
		Named("storagenodelatency").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
