// Cross-cluster authentication for this package's two reconcilers.
//
// A node's or node operation's control plane may be a
// ControlPlane.spec.source.managed one, on a different Kubernetes cluster
// than this operator, where a Kubernetes TokenReview of this operator's own
// service-account token can never succeed. What does cross that boundary is a
// cluster's own backend secret -- a plain credential the control plane's
// database matches by value, not a Kubernetes identity -- so every call
// scoped to an already-adopted cluster authenticates with that instead, once
// it is known. See internal/controllers/cluster's identically-motivated fix.

package node

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// clusterSecretByName reads the secret StorageClusterReconciler.persist wrote
// for the named StorageCluster, and reports the empty string when there is
// none.
func clusterSecretByName(
	ctx context.Context, c client.Client, namespace, clusterName string,
) (string, error) {
	var secret corev1.Secret
	key := types.NamespacedName{
		Name:      fmt.Sprintf("simplyblock-cluster-%s", clusterName),
		Namespace: namespace,
	}
	if err := c.Get(ctx, key, &secret); err != nil {
		return "", err
	}
	return string(secret.Data["secret"]), nil
}

// clusterSecretForNode reads the same secret for the cluster a named
// StorageNode belongs to, for a caller that only has the node's name (a
// StorageNodeOps names its target node, not the node's cluster directly).
func clusterSecretForNode(
	ctx context.Context, c client.Client, namespace, nodeRef string,
) (string, error) {
	var node simplyblockv1alpha2.StorageNode
	key := types.NamespacedName{Name: nodeRef, Namespace: namespace}
	if err := c.Get(ctx, key, &node); err != nil {
		return "", err
	}
	return clusterSecretByName(ctx, c, namespace, node.Spec.ClusterRef)
}

// authenticatedContext attaches a cluster's own credential to ctx when one is
// known, so a call scoped to it authenticates as that cluster instead of as
// this operator's Kubernetes identity.
func authenticatedContext(ctx context.Context, secret string, err error) context.Context {
	if err != nil || secret == "" {
		return ctx
	}
	return webapi.WithBearerToken(ctx, secret)
}
