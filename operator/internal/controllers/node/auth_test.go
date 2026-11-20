// A node's or node operation's control plane may be a
// ControlPlane.spec.source.managed one, on a different Kubernetes cluster
// than this operator, where a Kubernetes TokenReview of this operator's own
// service-account token can never succeed. Every call scoped to an
// already-adopted cluster must authenticate as that cluster's own recorded
// secret instead. See internal/controllers/cluster's identically-motivated
// tests.

package node

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// authTestClusterSecret is the cluster's own recorded credential, the one
// every assertion below wants to see instead of an operator SA token.
const authTestClusterSecret = "the-clusters-own-secret"

// aClusterSecret is the Secret StorageClusterReconciler.persist writes for
// anOpsCluster(), keyed by its Kubernetes name.
func aClusterSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + opsCluster,
			Namespace: opsNamespace,
		},
		Data: map[string][]byte{"secret": []byte(authTestClusterSecret)},
	}
}

// checkBearerTokens fails the test on any call the control plane recorded
// that did not carry authTestClusterSecret as its bearer-token override.
func checkBearerTokens(t *testing.T, api *scriptedControlPlane) {
	t.Helper()
	if len(api.calls) == 0 {
		t.Fatal("the control plane was never asked anything")
	}
	for i, obs := range api.bearerTokens {
		if !obs.ok {
			t.Errorf("call %d (%s): carried no bearer-token override", i, api.calls[i])
			continue
		}
		if obs.token != authTestClusterSecret {
			t.Errorf("call %d (%s): bearer token = %q, want the cluster's own secret %q",
				i, api.calls[i], obs.token, authTestClusterSecret)
		}
	}
}

// The entity reconciler's steady-state read (StorageNode()) authenticates as
// the node's cluster once that cluster's secret is on record.
func TestANodeReadAuthenticatesAsItsClusterOnceItsSecretIsKnown(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusInCreation)
	r, _ := aSteadyNode(t, api, aClusterSecret())
	// Forces the direct StorageNode() read rather than the stream's cache.
	r.Nodes = &deliveredNodes{synced: false}

	settle(t, r)

	checkBearerTokens(t, api)
}

// A node operation's calls -- the ones actions.go, classify.go,
// hostmaintenance.go, migrate.go, and remove.go make -- authenticate the same
// way, resolved from the operation's own target node's cluster.
func TestANodeOperationAuthenticatesAsItsClusterOnceItsSecretIsKnown(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusOnline)
	ops := anAdvancingOperation(
		"a-shutdown", simplyblockv1alpha2.StorageNodeOpsActionShutdown, stepRequesting)
	r, _ := anOpsWorld(t, api, ops, aClusterSecret())

	pass(t, r, "a-shutdown")

	checkBearerTokens(t, api)
}
