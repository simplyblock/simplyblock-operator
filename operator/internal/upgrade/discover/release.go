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

// address resolves where an object of the release actually lives.
//
// A manifest need not say. Helm renders a namespaced object without a namespace
// and lets it inherit the release's, which is what `helm install -n` means, so
// a reference built straight from the manifest names a namespaced object with
// no namespace and the API server refuses to look it up. Which of the two a
// kind is cannot be read from the manifest either, since a cluster-scoped
// object legitimately carries no namespace, so the API server is asked.
//
// It reports false for a kind the cluster does not serve, which is a chart that
// installed something whose CRD has since gone.
func address(s *upgrade.Scope, ref release.ObjectRef, fallback string) (types.NamespacedName, bool, error) {
	mapping, err := s.Client.RESTMapper().RESTMapping(ref.GVK.GroupKind(), ref.GVK.Version)
	if meta.IsNoMatchError(err) {
		return types.NamespacedName{}, false, nil
	}
	if err != nil {
		return types.NamespacedName{}, false, err
	}

	if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
		// Cluster-scoped, so a namespace the manifest happened to carry names
		// nothing and the API server refuses it.
		return types.NamespacedName{Name: ref.Name}, true, nil
	}

	namespace := ref.Namespace
	if namespace == "" {
		namespace = fallback
	}
	return types.NamespacedName{Namespace: namespace, Name: ref.Name}, true, nil
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
		key, addressable, err := address(s, ref, deployed.Namespace)
		if err != nil {
			return fmt.Errorf("locating %s from the release: %w", ref, err)
		}
		if !addressable {
			// A kind this cluster no longer serves. There is nothing to hand
			// over, and nothing to read it as.
			missing++
			continue
		}

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(ref.GVK)
		if err := s.Client.Get(ctx, key, obj); err != nil {
			if apierrors.IsNotFound(err) {
				// Deleted out of band, so the handover has nothing to annotate.
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
