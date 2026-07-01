package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch

// VolumeMigrationReconciler reconciles VolumeMigration resources.
type VolumeMigrationReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	apiClient  *webapi.Client
	coreClient corev1client.CoreV1Interface
}

func (r *VolumeMigrationReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	vm := &simplyblockv1alpha1.VolumeMigration{}
	if err := r.Get(ctx, req.NamespacedName, vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal phases — nothing left to do.
	switch vm.Status.Phase {
	case simplyblockv1alpha1.VolumeMigrationPhaseCompleted,
		simplyblockv1alpha1.VolumeMigrationPhaseFailed,
		simplyblockv1alpha1.VolumeMigrationPhaseAborted:
		return ctrl.Result{}, nil
	}

	// Abort is valid during Validating or Running.
	if vm.Spec.Abort {
		switch vm.Status.Phase {
		case simplyblockv1alpha1.VolumeMigrationPhaseValidating,
			simplyblockv1alpha1.VolumeMigrationPhaseRunning:
			return r.reconcileAbort(ctx, vm)
		}
	}

	switch vm.Status.Phase {
	case simplyblockv1alpha1.VolumeMigrationPhaseRunning:
		return r.reconcileRunning(ctx, vm)
	case simplyblockv1alpha1.VolumeMigrationPhaseValidating:
		return r.reconcileValidating(ctx, vm)
	default:
		return r.reconcileStart(ctx, vm)
	}
}

// reconcileStart resolves the PV to a logical volume, finds its cluster/pool,
// and submits the migration to the storage API.
func (r *VolumeMigrationReconciler) reconcileStart(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Resolve PV → volume UUID via CSI volume handle.
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, types.NamespacedName{Name: vm.Spec.PVName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return r.setFailed(ctx, vm, fmt.Sprintf("PersistentVolume %q not found", vm.Spec.PVName))
		}
		return ctrl.Result{}, fmt.Errorf("get PV %q: %w", vm.Spec.PVName, err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		return r.setFailed(ctx, vm, fmt.Sprintf("PV %q has no CSI volume handle", vm.Spec.PVName))
	}
	// CSI volume handle format: "<clusterUUID>:<poolUUID>:<volumeUUID>"
	parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return r.setFailed(ctx, vm, fmt.Sprintf("PV %q has unexpected CSI volume handle format %q (expected <clusterUUID>:<poolUUID>:<volumeUUID>)", vm.Spec.PVName, pv.Spec.CSI.VolumeHandle))
	}
	clusterUUID, poolUUID, volumeUUID := parts[0], parts[1], parts[2]

	if _, err := r.resolveRebalancerImage(ctx, vm.Namespace, clusterUUID); err != nil {
		return r.setFailed(ctx, vm, fmt.Sprintf("volume migration not enabled/configured for cluster %q: %v", clusterUUID, err))
	}

	log.Info("Submitting volume migration",
		"volume", volumeUUID, "cluster", clusterUUID, "target", vm.Spec.TargetNodeUUID)

	migration, err := r.apiClient.CreateMigration(ctx, clusterUUID, poolUUID, volumeUUID, vm.Spec.TargetNodeUUID)
	if err != nil {
		return r.setFailed(ctx, vm, fmt.Sprintf("CreateMigration: %v", err))
	}
	if migration.ID == "" {
		return r.setFailed(ctx, vm, "CreateMigration returned empty migration UUID")
	}

	now := metav1.Now()
	conns := make([]simplyblockv1alpha1.MigrationConnection, 0, len(migration.ConnectStrings))
	for _, c := range migration.ConnectStrings {
		conns = append(conns, simplyblockv1alpha1.MigrationConnection{
			NQN:            c.Nqn,
			IP:             c.IP,
			Port:           c.Port,
			Transport:      c.TargetType,
			NrIoQueues:     c.NrIoQueues,
			ReconnectDelay: c.ReconnectDelay,
			CtrlLossTmo:    c.CtrlLossTmo,
			FastIOFailTmo:  c.FastIOFailTmo,
			KeepAliveTmo:   c.KeepAliveTmo,
		})
	}

	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseValidating
	vm.Status.MigrationUUID = migration.ID
	vm.Status.ClusterUUID = clusterUUID
	vm.Status.VolumeUUID = volumeUUID
	vm.Status.PoolUUID = poolUUID
	// SourceNodeUUID and SnapsTotal are populated from GetMigration once status=Running.
	vm.Status.Connections = conns
	vm.Status.StartedAt = &now
	if err := r.Status().Patch(ctx, vm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status Validating: %w", err)
	}

	r.Recorder.Eventf(vm, corev1.EventTypeNormal, "MigrationCreated",
		"Migration %s created: validating %d connection(s) to node %s",
		migration.ID, len(conns), vm.Spec.TargetNodeUUID)
	return ctrl.Result{Requeue: true}, nil
}

// reconcileValidating creates a Job on the target worker node that:
//  1. Runs `nvme connect` for each connection returned by CreateMigration.
//  2. Runs `nvme list --verbose` and verifies all new NQNs appear with ANA
//     state "inaccessible" (paths connected but volume not yet migrated).
//
// On Job success the controller calls ContinueMigration and advances to Running.
// On Job failure the migration is cancelled.
func (r *VolumeMigrationReconciler) reconcileValidating(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if vm.Status.MigrationUUID == "" {
		return r.setFailed(ctx, vm, "migration UUID is empty in Validating phase; status was likely written before a failed CreateMigration")
	}

	// If the Job already exists, poll it.
	if vm.Status.ValidationJobName != "" {
		return r.pollValidationJob(ctx, vm)
	}

	// Resolve the k8s node name of the worker running the pod that uses the PVC.
	// NVMe connections must be established from that node, not the storage target node.
	hostname, err := r.resolveConsumerNodeName(ctx, vm.Spec.PVName)
	if err != nil {
		log.Error(err, "Cannot resolve consumer node name; requeuing")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Get the simplyblock-rebalancer image from the StorageCluster (it contains nvme-cli).
	image, err := r.resolveRebalancerImage(ctx, vm.Namespace, vm.Status.ClusterUUID)
	if err != nil {
		log.Error(err, "Cannot resolve simplyblock-rebalancer image; requeuing")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	job := r.buildValidationJob(vm, hostname, image)
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("create validation job: %w", err)
	}

	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.ValidationJobName = job.Name
	if err := r.Status().Patch(ctx, vm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch ValidationJobName: %w", err)
	}

	log.Info("Validation job created", "job", job.Name, "node", hostname,
		"connections", len(vm.Status.Connections))
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *VolumeMigrationReconciler) pollValidationJob(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: vm.Namespace, Name: vm.Status.ValidationJobName}, job); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get validation job: %w", err)
		}
		// The Job vanished before we observed a terminal state (TTL controller,
		// manual deletion, eviction, ...). Clear the recorded name and requeue so
		// reconcileValidating rebuilds it instead of getting wedged in Validating.
		log.Info("Validation job no longer exists; recreating",
			"job", vm.Status.ValidationJobName, "migration", vm.Status.MigrationUUID)
		patch := client.MergeFrom(vm.DeepCopy())
		vm.Status.ValidationJobName = ""
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("clear ValidationJobName: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Determine terminal state from Job conditions (set by the Job controller).
	var succeeded, failed bool
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			succeeded = true
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			failed = true
		}
	}
	if !succeeded && !failed {
		// Job still in progress — we will be re-triggered via Owns(&batchv1.Job{}).
		return ctrl.Result{}, nil
	}
	r.collectAndLogJobPodLogs(ctx, job)
	_ = r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))

	if failed {
		log.Error(nil, "Validation job failed; cancelling migration",
			"job", vm.Status.ValidationJobName, "migration", vm.Status.MigrationUUID)
		_ = r.apiClient.CancelMigration(ctx, vm.Status.ClusterUUID, vm.Status.PoolUUID, vm.Status.VolumeUUID, vm.Status.MigrationUUID)
		return r.setFailed(ctx, vm, "NVMe path validation failed; migration cancelled")
	}

	log.Info("Validation job succeeded; calling ContinueMigration",
		"migration", vm.Status.MigrationUUID)

	if err := r.apiClient.ContinueMigration(ctx, vm.Status.ClusterUUID, vm.Status.PoolUUID, vm.Status.VolumeUUID, vm.Status.MigrationUUID); err != nil {
		_ = r.apiClient.CancelMigration(ctx, vm.Status.ClusterUUID, vm.Status.PoolUUID, vm.Status.VolumeUUID, vm.Status.MigrationUUID)
		return r.setFailed(ctx, vm, fmt.Sprintf("ContinueMigration: %v", err))
	}

	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseRunning
	vm.Status.Connections = nil
	vm.Status.ValidationJobName = ""
	if err := r.Status().Patch(ctx, vm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status Running: %w", err)
	}

	r.Recorder.Eventf(vm, corev1.EventTypeNormal, "MigrationStarted",
		"Migration %s started: volume %s → node %s",
		vm.Status.MigrationUUID, vm.Status.VolumeUUID, vm.Spec.TargetNodeUUID)
	return ctrl.Result{RequeueAfter: vmigration.MigrationInitialDelay}, nil
}

// buildValidationJob constructs the Job that connects NVMe paths and validates
// ANA state on the target node using the simplyblock-rebalancer binary.
func (r *VolumeMigrationReconciler) buildValidationJob(
	vm *simplyblockv1alpha1.VolumeMigration,
	hostname, image string,
) *batchv1.Job {
	privileged := true
	ttl := int32(3600)
	backoffLimit := int32(0) // no retries — fail fast and cancel the migration

	connsJSON, _ := json.Marshal(connectionsToValidation(vm.Status.Connections))

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vmig-validate-" + safeNodeID(vm.Status.MigrationUUID),
			Namespace: vm.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(vm, simplyblockv1alpha1.GroupVersion.WithKind("VolumeMigration")),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  map[string]string{"kubernetes.io/hostname": hostname},
					HostNetwork:   true,
					Volumes: []corev1.Volume{
						{
							Name: "host-dev",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/dev"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "nvme-validate",
							Image:           image,
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"simplyblock-rebalancer", "--mode=validate-migration"},
							Env: []corev1.EnvVar{
								{Name: "VMIG_CONNECTIONS", Value: string(connsJSON)},
							},
							SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "host-dev", MountPath: "/dev"},
							},
						},
					},
				},
			},
		},
	}
}

// connectionsToValidation converts MigrationConnection status entries to the
// vmigration.Connection type consumed by the simplyblock-rebalancer validate-migration mode.
func connectionsToValidation(conns []simplyblockv1alpha1.MigrationConnection) []vmigration.Connection {
	out := make([]vmigration.Connection, len(conns))
	for i, c := range conns {
		out[i] = vmigration.Connection{
			NQN:            c.NQN,
			IP:             c.IP,
			Port:           c.Port,
			Transport:      c.Transport,
			NrIoQueues:     c.NrIoQueues,
			ReconnectDelay: c.ReconnectDelay,
			CtrlLossTmo:    c.CtrlLossTmo,
			FastIOFailTmo:  c.FastIOFailTmo,
			KeepAliveTmo:   c.KeepAliveTmo,
		}
	}
	return out
}

// safeNodeID produces a DNS-label-safe suffix from a node UUID.
func safeNodeID(nodeUUID string) string {
	s := strings.ReplaceAll(nodeUUID, "-", "")
	if len(s) > 20 {
		s = s[:20]
	}
	return s
}

// resolveConsumerNodeName finds the Kubernetes node name of the worker node
// running a pod that currently has the PVC (resolved from pvName) mounted.
// NVMe connections must be established from that node so that the consuming
// pod can reach the target subsystem after migration.
func (r *VolumeMigrationReconciler) resolveConsumerNodeName(
	ctx context.Context,
	pvName string,
) (string, error) {
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, types.NamespacedName{Name: pvName}, pv); err != nil {
		return "", fmt.Errorf("get PV %q: %w", pvName, err)
	}
	if pv.Spec.ClaimRef == nil {
		return "", fmt.Errorf("PV %q has no claimRef; volume may not be bound", pvName)
	}
	pvcName := pv.Spec.ClaimRef.Name
	pvcNamespace := pv.Spec.ClaimRef.Namespace

	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(pvcNamespace)); err != nil {
		return "", fmt.Errorf("list pods in namespace %q: %w", pvcNamespace, err)
	}
	for _, pod := range podList.Items {
		if pod.Spec.NodeName == "" || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == pvcName {
				return pod.Spec.NodeName, nil
			}
		}
	}
	return "", fmt.Errorf("no running pod found using PVC %q/%q", pvcNamespace, pvcName)
}

// collectAndLogJobPodLogs fetches stdout/stderr from every pod that belongs to
// the given Job and emits them as operator log lines. Must be called before
// deleting the Job, since pod deletion follows immediately after.
func (r *VolumeMigrationReconciler) collectAndLogJobPodLogs(ctx context.Context, job *batchv1.Job) {
	log := logf.FromContext(ctx).WithValues("job", job.Name)

	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name},
	); err != nil {
		log.Error(err, "Failed to list validation job pods for log collection")
		return
	}
	for _, pod := range podList.Items {
		req := r.coreClient.Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{})
		stream, err := req.Stream(ctx)
		if err != nil {
			log.Error(err, "Failed to stream logs from validation pod", "pod", pod.Name)
			continue
		}
		buf := new(bytes.Buffer)
		_, _ = io.Copy(buf, stream)
		_ = stream.Close()
		log.Info("Validation job pod output", "pod", pod.Name, "logs", buf.String())
	}
}

// defaultRebalancerImage is used when a StorageCluster enables volume migration
// (explicitly, or by default via an omitted settings block) without pinning a
// specific rebalancer image. The image must include nvme-cli.
const defaultRebalancerImage = "docker.io/simplyblock/simplyblock-rebalancer:main"

// resolveRebalancerImage returns the simplyblock-rebalancer image for the StorageCluster
// that owns the migration's volume. Volume migration is enabled by default: an omitted
// VolumeMigrationSettings block (or one without an image) resolves to defaultRebalancerImage;
// only an explicit Enabled=false disables it.
func (r *VolumeMigrationReconciler) resolveRebalancerImage(
	ctx context.Context,
	namespace, clusterUUID string,
) (string, error) {
	var clusters simplyblockv1alpha1.StorageClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list StorageClusters: %w", err)
	}
	for _, cr := range clusters.Items {
		if cr.Status.UUID != clusterUUID {
			continue
		}
		vm := cr.Spec.VolumeMigrationSettings
		if vm == nil {
			// No settings block: volume migration is enabled by default with the default image.
			return defaultRebalancerImage, nil
		}
		if vm.Enabled != nil && !*vm.Enabled {
			return "", fmt.Errorf("volume migration is disabled for cluster UUID %q", clusterUUID)
		}
		if vm.RebalancerImage != nil && *vm.RebalancerImage != "" {
			return *vm.RebalancerImage, nil
		}
		// Enabled (explicitly or by default) but no image pinned: use the default image.
		return defaultRebalancerImage, nil
	}
	return "", fmt.Errorf("no StorageCluster found for cluster UUID %q", clusterUUID)
}

// reconcileRunning polls the migration API and updates progress in status.
func (r *VolumeMigrationReconciler) reconcileRunning(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// StartedAt is an optional pointer and may be nil on older objects, manual
	// edits, or partial status writes. Backfill it rather than dereferencing a
	// nil pointer (panic) or defaulting to the zero time (instant "stuck" warning).
	if vm.Status.StartedAt == nil {
		now := metav1.Now()
		patch := client.MergeFrom(vm.DeepCopy())
		vm.Status.StartedAt = &now
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("backfill StartedAt: %w", err)
		}
		log.Info("StartedAt was unset in Running phase; backfilled", "migration", vm.Status.MigrationUUID)
	}

	migrationStart := vm.Status.StartedAt.Time
	result, err := vmigration.PollMigration(ctx, r.apiClient, vm.Status.ClusterUUID, vm.Status.PoolUUID, vm.Status.VolumeUUID, vm.Status.MigrationUUID, migrationStart)
	if err != nil {
		log.Error(err, "Cannot poll migration; requeuing", "migration", vm.Status.MigrationUUID)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if result.Migration != nil {
		// Update progress fields even if not done yet.
		patch := client.MergeFrom(vm.DeepCopy())
		vm.Status.SourceNodeUUID = result.Migration.SourceNodeID
		vm.Status.SnapsTotal = result.Migration.SnapsTotal
		vm.Status.SnapsMigrated = result.Migration.SnapsMigrated
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch progress: %w", err)
		}
	}

	if result.Stuck {
		r.Recorder.Eventf(vm, corev1.EventTypeWarning, "MigrationStuck",
			"Migration %s has not completed after 30 minutes (phase: %s, status: %s)",
			vm.Status.MigrationUUID, result.Migration.Phase, result.Migration.Status)
	}

	if !result.Done {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Migration finished.
	now := metav1.Now()
	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.CompletedAt = &now
	vm.Status.SnapsMigrated = result.Migration.SnapsMigrated
	if result.Succeeded {
		vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseCompleted
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch status Completed: %w", err)
		}
		r.Recorder.Eventf(vm, corev1.EventTypeNormal, "MigrationCompleted",
			"Migration %s completed successfully", vm.Status.MigrationUUID)
	} else {
		vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseFailed
		vm.Status.ErrorMessage = result.Migration.ErrorMessage
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch status Failed: %w", err)
		}
		r.Recorder.Eventf(vm, corev1.EventTypeWarning, "MigrationFailed",
			"Migration %s failed: %s", vm.Status.MigrationUUID, result.Migration.ErrorMessage)
	}
	return ctrl.Result{}, nil
}

// reconcileAbort cancels an in-progress migration.
func (r *VolumeMigrationReconciler) reconcileAbort(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Aborting migration", "migration", vm.Status.MigrationUUID)

	if err := r.apiClient.CancelMigration(ctx, vm.Status.ClusterUUID, vm.Status.PoolUUID, vm.Status.VolumeUUID, vm.Status.MigrationUUID); err != nil {
		log.Error(err, "CancelMigration failed; requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	now := metav1.Now()
	patch := client.MergeFrom(vm.DeepCopy())

	// Best-effort cleanup of the validation Job when aborting during Validating.
	if vm.Status.ValidationJobName != "" {
		_ = r.Delete(ctx,
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: vm.Status.ValidationJobName, Namespace: vm.Namespace}},
			client.PropagationPolicy(metav1.DeletePropagationBackground),
		)
		vm.Status.ValidationJobName = ""
		vm.Status.Connections = nil
	}

	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseAborted
	vm.Status.CompletedAt = &now
	if err := r.Status().Patch(ctx, vm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status Aborted: %w", err)
	}
	r.Recorder.Eventf(vm, corev1.EventTypeNormal, "MigrationAborted",
		"Migration %s cancelled", vm.Status.MigrationUUID)
	return ctrl.Result{}, nil
}

// setFailed transitions the migration to Failed with the given reason.
func (r *VolumeMigrationReconciler) setFailed(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
	reason string,
) (ctrl.Result, error) {
	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseFailed
	vm.Status.ErrorMessage = reason
	if err := r.Status().Patch(ctx, vm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status Failed: %w", err)
	}
	r.Recorder.Eventf(vm, corev1.EventTypeWarning, "MigrationFailed", reason)
	return ctrl.Result{}, nil
}

func (r *VolumeMigrationReconciler) SetupWithManager(
	mgr ctrl.Manager,
) error {
	r.apiClient = webapi.NewClient()
	k8s, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("create k8s client for log collection: %w", err)
	}
	r.coreClient = k8s.CoreV1()
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.VolumeMigration{}).
		Owns(&batchv1.Job{}).
		Named("volumemigration").
		Complete(r)
}
