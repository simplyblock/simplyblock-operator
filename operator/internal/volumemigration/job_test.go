package volumemigration

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	testNamespace   = "simplyblock"
	testClusterUUID = "11111111-1111-1111-1111-111111111111"

	// operatorImageEnv is the variable the chart sets to the operator's own
	// image, and releaseOperatorImage what a release install sets it to.
	operatorImageEnv       = "SB_OPERATOR_IMAGE"
	releaseOperatorImage   = "quay.io/simplyblock-io/simplyblock-operator:v26.4.0-beta2"
	releaseRebalancerImage = "quay.io/simplyblock-io/simplyblock-rebalancer:v26.4.0-beta2"
)

// clusterReporting builds a StorageCluster reporting testClusterUUID with the
// given migration settings.
func clusterReporting(settings *simplyblockv1alpha2.VolumeMigrationSettings) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: testNamespace},
		Spec:       simplyblockv1alpha2.StorageClusterSpec{VolumeMigrationSettings: settings},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: testClusterUUID},
	}
}

// Regression: 2026-10-20-job-image-main-fallback — a cluster that pinned no
// rebalancer image ran every validation, release, and replication Job on
// docker.io/simplyblock/simplyblock-rebalancer:main, whatever release the
// operator was. A v26.4.0-beta2 install ran migration Jobs from the moving
// development tag, on Docker Hub rather than the registry the release was
// installed from.
func TestJobImage(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	t.Run("the cluster's pinned image is used", func(t *testing.T) {
		t.Setenv(operatorImageEnv, releaseOperatorImage)
		t.Setenv(RebalancerImageEnvVar, "registry.example.com/ignored:v9")
		const pinned = "pinned:v1"
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			clusterReporting(&simplyblockv1alpha2.VolumeMigrationSettings{
				RebalancerImage: ptr.To(pinned),
			})).Build()

		got, err := JobImage(context.Background(), c, testNamespace, testClusterUUID)
		if err != nil {
			t.Fatal(err)
		}
		if got != pinned {
			t.Errorf("image = %q, want the pinned one", got)
		}
	})

	t.Run("a cluster pinning none runs the operator's release", func(t *testing.T) {
		t.Setenv(operatorImageEnv, releaseOperatorImage)
		t.Setenv(RebalancerImageEnvVar, "")
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			clusterReporting(&simplyblockv1alpha2.VolumeMigrationSettings{})).Build()

		got, err := JobImage(context.Background(), c, testNamespace, testClusterUUID)
		if err != nil {
			t.Fatal(err)
		}
		if got != releaseRebalancerImage {
			t.Errorf("image = %q, want %q", got, releaseRebalancerImage)
		}
	})

	t.Run("a cluster pinning none takes the deployment's rebalancer image", func(t *testing.T) {
		t.Setenv(operatorImageEnv, releaseOperatorImage)
		t.Setenv(RebalancerImageEnvVar, "registry.example.com/rebalancer:v9")
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterReporting(nil)).Build()

		got, err := JobImage(context.Background(), c, testNamespace, testClusterUUID)
		if err != nil {
			t.Fatal(err)
		}
		if got != "registry.example.com/rebalancer:v9" {
			t.Errorf("image = %q, want the deployment's", got)
		}
	})

	t.Run("a cluster that is not there runs the operator's release", func(t *testing.T) {
		t.Setenv(operatorImageEnv, releaseOperatorImage)
		t.Setenv(RebalancerImageEnvVar, "")
		c := fake.NewClientBuilder().WithScheme(scheme).Build()

		got, err := JobImage(context.Background(), c, testNamespace, testClusterUUID)
		if err != nil {
			t.Fatal(err)
		}
		if got != releaseRebalancerImage {
			t.Errorf("image = %q, want %q", got, releaseRebalancerImage)
		}
	})

	// With nothing to derive the image from, a Job is not created at all. Any
	// literal default is some release other than the operator's.
	t.Run("nothing naming an image is an error", func(t *testing.T) {
		t.Setenv(operatorImageEnv, "")
		t.Setenv(RebalancerImageEnvVar, "")
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterReporting(nil)).Build()

		got, err := JobImage(context.Background(), c, testNamespace, testClusterUUID)
		if err == nil {
			t.Fatalf("image = %q, want an error naming what to set", got)
		}
		for _, name := range []string{operatorImageEnv, RebalancerImageEnvVar} {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name %s", err, name)
			}
		}
	})
}

// TestBuildJobPinsTheNodeAndReadsTheHost. Everything the modes have in common
// is here rather than in each caller, because they are the same binary reading
// the same host state: a Job that saw a different fabric than its neighbor
// would be deciding about paths it cannot see.
func TestBuildJobPinsTheNodeAndReadsTheHost(t *testing.T) {
	job := BuildJob(JobParams{
		Name:          "vmig-validate-worker-3",
		Namespace:     testNamespace,
		Hostname:      "worker-3",
		Image:         "rebalancer:v1",
		ContainerName: "nvme-validate",
		Mode:          "validate-migration",
		Env:           []corev1.EnvVar{{Name: "VMIG_SYS_ROOT", Value: "/host/sys"}},
		BackoffLimit:  0,
		TTL:           3600,
		Deadline:      180,
	})

	pod := job.Spec.Template.Spec
	if pod.NodeSelector["kubernetes.io/hostname"] != "worker-3" {
		t.Errorf("the Job is not pinned to the node whose fabric it reads: %v", pod.NodeSelector)
	}
	if !pod.HostNetwork {
		t.Error("the Job does not share the host's network, so it cannot reach the target")
	}
	container := pod.Containers[0]
	if got := container.Command; len(got) != 2 || got[1] != "--mode=validate-migration" {
		t.Errorf("command = %v, want the mode it was asked for", got)
	}

	// The container's own /sys is not the host's, and every mode reads the
	// host's NVMe fabric out of it.
	var mounted bool
	for _, m := range container.VolumeMounts {
		if m.MountPath == "/host/sys" {
			mounted = true
			if !m.ReadOnly {
				t.Error("the host's sysfs is mounted writable")
			}
		}
	}
	if !mounted {
		t.Error("the host's sysfs is not mounted, so the Job reads its own")
	}
}

// TestBuildJobWithoutAnOwnerCarriesNoReference. A cluster-scoped operation
// cannot own a namespaced Job: Kubernetes treats the reference as unresolvable
// and garbage-collects the dependent, which here would delete the Job while it
// is checking a path.
func TestBuildJobWithoutAnOwnerCarriesNoReference(t *testing.T) {
	job := BuildJob(JobParams{Name: "j", Namespace: testNamespace, Hostname: "worker-3"})
	if len(job.OwnerReferences) != 0 {
		t.Errorf("owner references = %v, want none", job.OwnerReferences)
	}
}

// clusterTolerating builds a StorageCluster whose storage nodes tolerate one
// taint, which is what a fleet with a dedicated storage plane looks like.
func clusterTolerating(tolerations []corev1.Toleration) *simplyblockv1alpha2.StorageCluster {
	cluster := clusterReporting(nil)
	cluster.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{Tolerations: tolerations}
	return cluster
}

func TestJobPlacementCarriesTheClustersTolerations(t *testing.T) {
	t.Setenv(operatorImageEnv, releaseOperatorImage)
	t.Setenv(RebalancerImageEnvVar, "")
	// A validation Job is pinned to the node whose paths it checks. On a
	// hyper-converged fleet that node is a tainted storage worker, and a Job
	// tolerating nothing stays Pending until its deadline kills it, which
	// leaves the migration's paths connected and nothing to disconnect them.
	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	tolerations := []corev1.Toleration{{
		Key:      "io.simplyblock.node-type",
		Operator: corev1.TolerationOpEqual,
		Value:    "storage-plane",
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(clusterTolerating(tolerations)).Build()

	placement, err := JobPlacementOf(context.Background(), c, testNamespace, testClusterUUID)
	if err != nil {
		t.Fatalf("read the placement: %v", err)
	}
	if len(placement.Tolerations) != 1 || placement.Tolerations[0].Key != "io.simplyblock.node-type" {
		t.Errorf("the placement tolerates %+v, want the cluster's taint", placement.Tolerations)
	}
	if placement.Image != releaseRebalancerImage {
		t.Errorf("the placement runs %q, want %q", placement.Image, releaseRebalancerImage)
	}

	job := BuildJob(JobParams{
		Name: "check", Namespace: testNamespace, Hostname: "worker-1",
		Image: placement.Image, Tolerations: placement.Tolerations,
	})
	// The cluster's taint first; the pinned node's NoSchedule taints are
	// tolerated on top (TestBuildJobToleratesThePinnedNodesNoScheduleTaints).
	if tols := job.Spec.Template.Spec.Tolerations; len(tols) == 0 || tols[0] != tolerations[0] {
		t.Errorf("the Job tolerates %+v, want the cluster's taint", tols)
	}
}

func TestJobPlacementOfAClusterThatTakesNoTaint(t *testing.T) {
	t.Setenv(operatorImageEnv, releaseOperatorImage)
	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterReporting(nil)).Build()

	placement, err := JobPlacementOf(context.Background(), c, testNamespace, testClusterUUID)
	if err != nil {
		t.Fatalf("read the placement: %v", err)
	}
	if len(placement.Tolerations) != 0 {
		t.Errorf("the placement tolerates %+v with nothing stated", placement.Tolerations)
	}
}

func TestBuildJobRunsTheRebalancerAsRoot(t *testing.T) {
	job := BuildJob(JobParams{Name: "j", Namespace: testNamespace, Hostname: "worker-3"})
	sc := job.Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil || sc.Privileged == nil || !*sc.Privileged {
		t.Fatalf("the container must be privileged: %+v", sc)
	}
	// The container calls nvme-cli directly as root, as the CSI node plugin does; the
	// sudo hop a non-root uid needed failed PAM inside a privileged container on GKE.
	if sc.RunAsUser == nil || *sc.RunAsUser != 0 || sc.RunAsGroup == nil || *sc.RunAsGroup != 0 {
		t.Fatalf("the container must run as root (uid 0, gid 0): user=%v group=%v", sc.RunAsUser, sc.RunAsGroup)
	}
}

// TestBuildJobToleratesThePinnedNodesNoScheduleTaints. A preconnect or
// validation Job is pinned to the node that already runs the consuming pod, so
// that node's NoSchedule taints are the workload's own. Carrying only the
// storage nodes' tolerations left the Job Pending on a tainted client worker,
// and the backend's cutover-proceed timeout then flipped ANA with no target
// path connected. A NoExecute taint (the node is going away) is not tolerated.
func TestBuildJobToleratesThePinnedNodesNoScheduleTaints(t *testing.T) {
	storage := corev1.Toleration{Key: "storage.simplyblock.io/dedicated", Operator: corev1.TolerationOpEqual,
		Value: "true", Effect: corev1.TaintEffectNoSchedule}
	job := BuildJob(JobParams{Name: "j", Namespace: testNamespace, Hostname: "worker-3",
		Tolerations: []corev1.Toleration{storage}})

	clientTaint := corev1.Taint{Key: "simplyblock.io/role", Value: "client", Effect: corev1.TaintEffectNoSchedule}
	dying := corev1.Taint{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute}
	var tolerated, toleratesDying bool
	for i := range job.Spec.Template.Spec.Tolerations {
		tol := job.Spec.Template.Spec.Tolerations[i]
		tolerated = tolerated || tolerates(tol, clientTaint)
		toleratesDying = toleratesDying || tolerates(tol, dying)
	}
	if !tolerated {
		t.Errorf("tolerations %v keep the Job off a tainted client worker", job.Spec.Template.Spec.Tolerations)
	}
	if toleratesDying {
		t.Error("the Job tolerates NoExecute: it would stay on a node that is going away")
	}
	if job.Spec.Template.Spec.Tolerations[0] != storage {
		t.Error("the storage nodes' tolerations were dropped")
	}
}

// tolerates is the scheduler's toleration match for the Exists and Equal operators.
func tolerates(tol corev1.Toleration, taint corev1.Taint) bool {
	if tol.Effect != "" && tol.Effect != taint.Effect {
		return false
	}
	if tol.Key != "" && tol.Key != taint.Key {
		return false
	}
	if tol.Operator == corev1.TolerationOpExists {
		return true
	}
	return tol.Key == taint.Key && tol.Value == taint.Value
}
