// The draft checks, offered to a caller that holds no reconciler: the support
// tool, which reads a customer's support bundle and wants the verdict the
// operator would give a ClusterDeploymentConfig against the cluster the bundle
// captured.
//
// It is a separate entry point rather than an exported validate, because the
// controller's findings carry their reason as an unexported field so that a
// finding and its event cannot drift. The caller gets the same pairs, as values.

package deployment

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Finding is one problem the draft checks found: the reason its Warning event is
// raised under, and the message.
type Finding struct {
	Reason  string
	Message string
}

// ValidateDocument runs the checks the controller runs on every pass over a
// draft, reading the cluster through c, and returns every finding.
func ValidateDocument(
	ctx context.Context, c client.Client, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]Finding, error) {
	r := &ClusterDeploymentConfigReconciler{Client: c}
	found, err := r.validate(ctx, config)
	if err != nil {
		return nil, err
	}
	out := make([]Finding, 0, len(found))
	for _, f := range found {
		out = append(out, Finding{Reason: f.reason, Message: f.message})
	}
	return out, nil
}
