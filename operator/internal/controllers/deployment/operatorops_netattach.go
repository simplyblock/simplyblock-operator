// The check a run naming NetworkAttachmentDefinitions gets before anything is
// probed: every definition it names has to exist in its namespace.
//
// It lives in the controller rather than in the planner because it is a fact
// about the cluster, answered once from the API, and because it is the one
// answer that makes every probe pointless. A run whose definition does not
// exist cannot draft anything the administrator asked for, so it fails at once
// with the names the namespace does hold, rather than after a probe on every
// worker, under a message about devices. A cluster that does not serve the kind
// passes: the probes confirm it, and the draft keeps host networking.

package deployment

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/inventory"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// refuseMissingAttachments refuses a run naming a definition its namespace does
// not hold.
func (r *OperatorOpsReconciler) refuseMissingAttachments(
	ctx context.Context, namespace string, spec *simplyblockv1alpha2.DiscoverSpec,
) error {
	selection := spec.NetworkAttachment
	if selection == nil {
		return nil
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(inventory.NetworkAttachmentDefinitionResource.GroupVersion().
		WithKind("NetworkAttachmentDefinitionList"))
	switch err := r.List(ctx, list, client.InNamespace(namespace)); {
	case meta.IsNoMatchError(err):
		return nil
	case err != nil:
		return fmt.Errorf("listing the NetworkAttachmentDefinitions in %s: %w", namespace, err)
	}

	held := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		held = append(held, item.GetName())
	}
	slices.Sort(held)

	for _, name := range []string{selection.Management, selection.Data} {
		if name == "" || slices.Contains(held, name) {
			continue
		}
		holds := "The namespace holds none."
		if len(held) > 0 {
			holds = "It holds " + strings.Join(held, ", ") + "."
		}
		return refusef(OperationFailed,
			"spec.discover.networkAttachment names the NetworkAttachmentDefinition %s, which the namespace %s does not hold. %s",
			name, namespace, holds)
	}
	return nil
}
