package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch

// VolumeMigrationReconciler reconciles VolumeMigration resources.
type VolumeMigrationReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	apiClient *webapi.Client
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

	log.Info("Submitting volume migration",
		"volume", volumeUUID, "cluster", clusterUUID, "target", vm.Spec.TargetNodeUUID)

	migration, err := r.apiClient.CreateMigration(ctx, clusterUUID, poolUUID, volumeUUID, vm.Spec.TargetNodeUUID)
	if err != nil {
		return r.setFailed(ctx, vm, fmt.Sprintf("CreateMigration: %v", err))
	}
	if migration.ID == "" {
		return r.setFailed(ctx, vm, "CreateMigration returned empty migration ID")
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
		})
	}

	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseValidating
	vm.Status.MigrationID = migration.ID
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

	if vm.Status.MigrationID == "" {
		return r.setFailed(ctx, vm, "migration ID is empty in Validating phase; status was likely written before a failed CreateMigration")
	}

	// If the Job already exists, poll it.
	if vm.Status.ValidationJobName != "" {
		return r.pollValidationJob(ctx, vm)
	}

	// Resolve the k8s node name for the target storage node.
	hostname, err := r.resolveNodeHostname(ctx, vm.Namespace, vm.Spec.TargetNodeUUID)
	if err != nil {
		log.Error(err, "Cannot resolve target node hostname; requeuing")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Get the fio-bench image from the StorageCluster (it contains nvme-cli).
	image, err := r.resolveFioBenchImage(ctx, vm.Namespace, vm.Status.ClusterUUID)
	if err != nil {
		log.Error(err, "Cannot resolve fio-bench image; requeuing")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	job := r.buildValidationJob(vm, hostname, image)
	if err := r.Create(ctx, job); err != nil {
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
		return ctrl.Result{}, client.IgnoreNotFound(err)
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
	_ = r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))

	if failed {
		log.Error(nil, "Validation job failed; cancelling migration",
			"job", vm.Status.ValidationJobName, "migration", vm.Status.MigrationID)
		_ = r.apiClient.CancelMigration(ctx, vm.Status.ClusterUUID, vm.Status.PoolUUID, vm.Status.VolumeUUID, vm.Status.MigrationID)
		return r.setFailed(ctx, vm, "NVMe path validation failed; migration cancelled")
	}

	log.Info("Validation job succeeded; calling ContinueMigration",
		"migration", vm.Status.MigrationID)

	if _, err := r.apiClient.ContinueMigration(ctx, vm.Status.ClusterUUID, vm.Status.PoolUUID, vm.Status.VolumeUUID, vm.Status.MigrationID); err != nil {
		_ = r.apiClient.CancelMigration(ctx, vm.Status.ClusterUUID, vm.Status.PoolUUID, vm.Status.VolumeUUID, vm.Status.MigrationID)
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
		vm.Status.MigrationID, vm.Status.VolumeUUID, vm.Spec.TargetNodeUUID)
	return ctrl.Result{RequeueAfter: volumemigration.MigrationInitialDelay}, nil
}

// buildValidationJob constructs the Job that connects NVMe paths and validates
// ANA state on the target node.
func (r *VolumeMigrationReconciler) buildValidationJob(
	vm *simplyblockv1alpha1.VolumeMigration,
	hostname, image string,
) *batchv1.Job {
	privileged := true
	ttl := int32(3600)
	backoffLimit := int32(0) // no retries — fail fast and cancel the migration

	// Build the shell script: connect each path, then verify ANA state.
	script := buildNVMeValidationScript(vm.Status.Connections)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vmig-validate-" + safeNodeID(vm.Status.MigrationID),
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
							Command:         []string{"sh", "-c"},
							Args:            []string{script},
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

// buildNVMeValidationScript builds the shell script run inside the validation Job.
// It connects each NVMe path and then verifies all NQNs appear with ANA state
// "inaccessible" (connected to target but volume not yet migrated there).
func buildNVMeValidationScript(
	conns []simplyblockv1alpha1.MigrationConnection,
) string {
	var b strings.Builder
	b.WriteString("set -e\n")

	// Connect each path.
	for _, c := range conns {
		fmt.Fprintf(&b, "sudo nvme connect -t %s -a %s -s %d -n %s",
			c.Transport, c.IP, c.Port, c.NQN)
		if c.NrIoQueues > 0 {
			fmt.Fprintf(&b, " --nr-io-queues=%d", c.NrIoQueues)
		}
		if c.ReconnectDelay > 0 {
			fmt.Fprintf(&b, " --reconnect-delay=%d", c.ReconnectDelay)
		}
		if c.CtrlLossTmo > 0 {
			fmt.Fprintf(&b, " --ctrl-loss-tmo=%d", c.CtrlLossTmo)
		}
		b.WriteString("\n")
	}

	// Verify all NQNs appear with ANA state "inaccessible".
	b.WriteString("sleep 2\n") // give the kernel a moment to register the paths
	b.WriteString("sudo nvme list --verbose --output-format=json > /tmp/nvme_list.json\n")

	// Build the list of expected NQNs as a Python set literal.
	b.WriteString("python3 - <<'EOF'\n")
	b.WriteString("import json, sys\n")
	b.WriteString("data = json.load(open('/tmp/nvme_list.json'))\n")
	b.WriteString("expected = {")
	for i, c := range conns {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", c.NQN)
	}
	b.WriteString("}\n")
	b.WriteString("found = {}\n")
	b.WriteString("for dev in data.get('Devices', []):\n")
	b.WriteString("  for sub in dev.get('Subsystems', []):\n")
	b.WriteString("    if sub.get('SubsystemNQN') in expected:\n")
	b.WriteString("      found[sub['SubsystemNQN']] = sub.get('ANA_State', '')\n")
	b.WriteString("missing = expected - set(found)\n")
	b.WriteString("if missing:\n")
	b.WriteString("  print('Missing NQNs:', missing, file=sys.stderr); sys.exit(1)\n")
	b.WriteString("bad = {n: s for n, s in found.items() if s != 'inaccessible'}\n")
	b.WriteString("if bad:\n")
	b.WriteString("  print('Wrong ANA state:', bad, file=sys.stderr); sys.exit(1)\n")
	b.WriteString("print('All paths validated: ANA state inaccessible')\n")
	b.WriteString("EOF\n")

	return b.String()
}

// resolveNodeHostname scans StorageNode CRs to find the k8s node name
// for the given storage node UUID.
func (r *VolumeMigrationReconciler) resolveNodeHostname(
	ctx context.Context,
	namespace, nodeUUID string,
) (string, error) {
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list StorageNodes: %w", err)
	}
	for _, sn := range snList.Items {
		for _, n := range sn.Status.Nodes {
			if n.UUID == nodeUUID && n.Hostname != "" {
				return n.Hostname, nil
			}
		}
	}
	return "", fmt.Errorf("no StorageNode found with node UUID %q in namespace %q", nodeUUID, namespace)
}

// resolveFioBenchImage returns the fio benchmark image configured on the
// StorageCluster that owns the migration's volume.
func (r *VolumeMigrationReconciler) resolveFioBenchImage(
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
		rb := cr.Spec.VolumeRebalancing
		if rb != nil && rb.FioBenchmarkImage != nil && *rb.FioBenchmarkImage != "" {
			return *rb.FioBenchmarkImage, nil
		}
	}
	return "", fmt.Errorf("no FioBenchmarkImage configured for cluster UUID %q", clusterUUID)
}

// reconcileRunning polls the migration API and updates progress in status.
func (r *VolumeMigrationReconciler) reconcileRunning(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	migrationStart := vm.Status.StartedAt.Time
	result, err := volumemigration.PollMigration(ctx, r.apiClient, vm.Status.ClusterUUID, vm.Status.PoolUUID, vm.Status.VolumeUUID, vm.Status.MigrationID, migrationStart)
	if err != nil {
		log.Error(err, "Cannot poll migration; requeuing", "migration", vm.Status.MigrationID)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if result.Migration != nil {
		// Update progress fields even if not done yet.
		patch := client.MergeFrom(vm.DeepCopy())
		vm.Status.SnapsTotal = result.Migration.SnapsTotal
		vm.Status.SnapsMigrated = result.Migration.SnapsMigrated
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch progress: %w", err)
		}
	}

	if result.Stuck {
		r.Recorder.Eventf(vm, corev1.EventTypeWarning, "MigrationStuck",
			"Migration %s has not completed after 30 minutes (phase: %s, status: %s)",
			vm.Status.MigrationID, result.Migration.Phase, result.Migration.Status)
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
			"Migration %s completed successfully", vm.Status.MigrationID)
	} else {
		vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseFailed
		vm.Status.ErrorMessage = result.Migration.ErrorMessage
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch status Failed: %w", err)
		}
		r.Recorder.Eventf(vm, corev1.EventTypeWarning, "MigrationFailed",
			"Migration %s failed: %s", vm.Status.MigrationID, result.Migration.ErrorMessage)
	}
	return ctrl.Result{}, nil
}

// reconcileAbort cancels an in-progress migration.
func (r *VolumeMigrationReconciler) reconcileAbort(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Aborting migration", "migration", vm.Status.MigrationID)

	if err := r.apiClient.CancelMigration(ctx, vm.Status.ClusterUUID, vm.Status.PoolUUID, vm.Status.VolumeUUID, vm.Status.MigrationID); err != nil {
		log.Error(err, "CancelMigration failed; requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	now := metav1.Now()
	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseAborted
	vm.Status.CompletedAt = &now
	if err := r.Status().Patch(ctx, vm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status Aborted: %w", err)
	}
	r.Recorder.Eventf(vm, corev1.EventTypeNormal, "MigrationAborted",
		"Migration %s cancelled", vm.Status.MigrationID)
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.VolumeMigration{}).
		Owns(&batchv1.Job{}).
		Named("volumemigration").
		Complete(r)
}
