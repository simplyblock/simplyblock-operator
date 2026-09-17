package volumemigration

import (
	"context"
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

func TestJobImage(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	t.Run("the cluster's pinned image is used", func(t *testing.T) {
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

	t.Run("a cluster pinning none falls back", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			clusterReporting(&simplyblockv1alpha2.VolumeMigrationSettings{})).Build()

		got, err := JobImage(context.Background(), c, testNamespace, testClusterUUID)
		if err != nil {
			t.Fatal(err)
		}
		if got != JobImageDefault {
			t.Errorf("image = %q, want %q", got, JobImageDefault)
		}
	})

	t.Run("a cluster that is not there falls back", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()

		got, err := JobImage(context.Background(), c, testNamespace, testClusterUUID)
		if err != nil {
			t.Fatal(err)
		}
		if got != JobImageDefault {
			t.Errorf("image = %q, want %q", got, JobImageDefault)
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
