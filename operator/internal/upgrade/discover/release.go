// The objects the deployed Helm release installed, which §12 hands over.
//
// They are read because the handover acts on each of them, and a step's
// subjects come from the graph. Without them the riskiest step in the upgrade
// appears as one line where it is ninety-odd annotations, and the one thing a
// user checking that step wants to know is which objects it is about.
//
// They are fetched rather than taken from the manifest, because what the
// annotation goes on is the live object and §12.2 turns on Helm reading the
// annotation from the live object rather than from the stored manifest. An
// object the manifest names and the cluster no longer holds is skipped: it was
// deleted out of band, and the handover has nothing to annotate.

package discover

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/release"
)

// IDHelmRelease names the discoverer.
const IDHelmRelease upgrade.ID = "discover-helm-release"

// HelmRelease reads what the deployed release installed.
func HelmRelease() []upgrade.Discoverer {
	return []upgrade.Discoverer{helmRelease{}}
}

type helmRelease struct{}

func (helmRelease) ID() upgrade.ID         { return IDHelmRelease }
func (helmRelease) Requires() []upgrade.ID { return nil }

func (helmRelease) Description() string {
	return "reads the objects the deployed Helm release installed, which §12 hands over before the upgrade prunes them"
}

// Discover reads the release and adopts every object it still holds.
//
// A cluster with no Helm release is not an error. §13.2 has the OLM-installed
// shape, where the release does not exist and the handover has nothing to do,
// and that is how the two channels are told apart.
func (h helmRelease) Discover(ctx context.Context, s *upgrade.Scope) error {
	s.Report.Item("the deployed Helm release")

	deployed, found, err := release.Deployed(ctx, s.Client, s.Namespace)
	if err != nil {
		return err
	}
	if !found {
		s.Report.Progress("%s: no Helm release in %s, which is what an OLM-installed cluster looks like",
			IDHelmRelease, s.Namespace)
		return nil
	}

	var missing int
	for _, ref := range deployed.Sorted() {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(ref.GVK)

		key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
		if err := s.Client.Get(ctx, key, obj); err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				// Deleted out of band, or a kind this cluster no longer
				// serves. Either way there is nothing to hand over.
				missing++
				continue
			}
			return fmt.Errorf("reading %s from the release: %w", ref, err)
		}
		s.Adopt(obj)
	}

	s.Report.Progress("%s: %s holds %d object(s), %d of which the cluster no longer has",
		IDHelmRelease, deployed.Name, len(deployed.Objects), missing)
	return nil
}
