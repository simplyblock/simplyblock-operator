// Whether the cluster name a discovery run proposes carries something that
// distinguishes this Kubernetes cluster from another one pointed at the same
// control plane.
//
// InitialDiscoveryName is deliberately identical on every install, and that is
// fine for the OperatorOps and the ClusterDeploymentConfig discovery writes --
// both live in this Kubernetes cluster's own API server, where the name
// collides with nothing else. The StorageCluster the document proposes does
// not stay local: it is registered on the control plane by name
// (StorageClusterReconciler.creationParams), and two separate Kubernetes
// clusters pointed at the same control plane (ControlPlane.spec.source.managed)
// otherwise propose the identical one. StorageClusterReconciler.postCluster's
// create-conflict fallback exists to resume a retried create of the SAME
// cluster and cannot tell that apart from a name that belongs to an entirely
// different Kubernetes cluster's own -- so it silently adopts the other one.

package deployment

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// kubeSystemNamespace is the fixture that gives a fake cluster its own
// identity, the same as the real kube-system Namespace every Kubernetes
// cluster carries from the moment its API server first comes up.
func kubeSystemNamespace(uid types.UID) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: metav1.NamespaceSystem, UID: uid},
	}
}

// writtenClusterName drives a discovery run to completion and returns the
// cluster name its document proposes.
func writtenClusterName(t *testing.T, r *runner) string {
	t.Helper()
	r.step() // start
	r.step() // inspect
	r.step() // probing: creates the Jobs

	if err := r.client.Create(t.Context(),
		reportConfigMap(t, "worker-1", "0000:5e:00.0")); err != nil {
		t.Fatalf("write a report: %v", err)
	}

	r.step() // probing: sees the report, moves to Writing
	r.step() // writing

	configs := r.configs()
	if len(configs) != 1 {
		t.Fatalf("wrote %d documents, want 1", len(configs))
	}
	if configs[0].Spec.Cluster == nil {
		t.Fatalf("the document names no cluster template")
	}
	return configs[0].Spec.Cluster.Name
}

// Two Kubernetes clusters running the identical, unmodified bootstrap
// discovery -- same OperatorOps name, same fleet shape -- propose different
// cluster names once each has its own kube-system identity. This is the
// property that keeps a second cluster's discovery from ever being mistaken,
// on the shared control plane, for the first cluster's own.
func TestTwoKubernetesClustersProposeDifferentClusterNames(t *testing.T) {
	clusterA := newRunner(t,
		discoverRun(nil), worker("worker-1"), kubeSystemNamespace("11111111-1111-1111-1111-111111111111"))
	clusterB := newRunner(t,
		discoverRun(nil), worker("worker-1"), kubeSystemNamespace("22222222-2222-2222-2222-222222222222"))

	nameA := writtenClusterName(t, clusterA)
	nameB := writtenClusterName(t, clusterB)

	if nameA == nameB {
		t.Fatalf("both clusters proposed %q, which is exactly the collision this fixes", nameA)
	}
}

// The same kube-system identity proposes the same cluster name on a second
// run: the suffix is derived, not random, which is what keeps the "create is
// idempotent by name" property bootstrap.go documents.
func TestTheSameKubernetesClusterProposesTheSameNameAcrossRuns(t *testing.T) {
	const uid = types.UID("33333333-3333-3333-3333-333333333333")

	first := newRunner(t, discoverRun(nil), worker("worker-1"), kubeSystemNamespace(uid))
	second := newRunner(t, discoverRun(nil), worker("worker-1"), kubeSystemNamespace(uid))

	nameFirst := writtenClusterName(t, first)
	nameSecond := writtenClusterName(t, second)

	if nameFirst != nameSecond {
		t.Errorf("proposed %q then %q for the same kube-system identity", nameFirst, nameSecond)
	}
}

// Without a readable kube-system Namespace -- every fixture in this package
// before this file, and any real cluster whose RBAC has not yet caught up --
// the proposed name is exactly what it always was. This is what keeps the fix
// additive.
func TestWithNoKubeSystemNamespaceTheNameIsUnchanged(t *testing.T) {
	r := newRunner(t, discoverRun(nil), worker("worker-1"))

	got := writtenClusterName(t, r)

	if got != configNamePrefix+opsName+clusterNameSuffix {
		t.Errorf("proposed %q, want %q", got, configNamePrefix+opsName+clusterNameSuffix)
	}
}
