// A document that would put a second StorageCluster in a namespace is reported
// while it is still a draft.
//
// The admission webhook refuses the cluster either way, but it refuses it during
// CreatingCluster, which is after approval has made the document immutable: the
// step then retries to its deadline and the failure that lands names the
// deadline rather than the reason. A namespace that is already occupied is a
// fact about the world, which is what validation is for.

package deployment

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// theFixtureWorkers are the workers aDocument names, as nodes of the Kubernetes
// cluster, so that the only finding a case produces is the one it is about.
func theFixtureWorkers() []client.Object {
	return []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-2"}},
	}
}

func TestADraftCreatingASecondClusterInANamespaceIsReported(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.ClusterRef = ""
		c.Spec.Cluster.Name = "a-new-cluster"
	})
	occupant := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "already-there", Namespace: theNamespace},
	}

	found := only(t, findingsOf(t, config,
		append(theFixtureWorkers(), occupant)...), ClusterExists)
	for _, want := range []string{"already-there", theNamespace} {
		if !strings.Contains(found.message, want) {
			t.Errorf("the finding does not mention %q: %s", want, found.message)
		}
	}
}

// A growth document names the cluster that is already there, so the namespace
// being occupied is the precondition rather than the problem.
func TestAGrowthDocumentIsNotReportedForItsOwnCluster(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.Cluster = nil
		c.Spec.ClusterRef = theCluster
	})

	for _, found := range findingsOf(t, config,
		append(theFixtureWorkers(), aCluster(nil))...) {
		if found.reason == ClusterExists {
			t.Fatalf("a growth document was reported for its own cluster: %s", found.message)
		}
	}
}

// The document that created the cluster is re-validated on every pass after it
// has been expanded, and the cluster it made is not a reason to report it.
func TestTheDocumentThatMadeTheClusterIsNotReportedForIt(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = true
		c.Spec.ClusterRef = ""
		c.Status.ClusterRef = theCluster
	})

	for _, found := range findingsOf(t, config,
		append(theFixtureWorkers(), aCluster(nil))...) {
		if found.reason == ClusterExists {
			t.Fatalf("a document was reported for the cluster it created: %s", found.message)
		}
	}
}
