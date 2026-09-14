// Registration of the CRD conversion webhook, and the list of kinds it serves.
//
// The conversion itself lives beside the API types, in api/v1alpha1/*_conversion.go,
// because that is where the two shapes being converted are declared. What lives
// here is the wiring: which hub types the manager should serve, and the one place
// a kind is added to that list when it gains a v1alpha2.
//
// Every kind listed in ConvertedKinds must also be listed in the CA-bundle
// injection of cert.go, or the API server will not trust the webhook and every
// read of that kind fails. ConvertedKindCRDNames is what keeps the two in step.

package webhook

import (
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// convertedKind pairs a hub type with the name of the CRD that declares it. The
// CRD name is what the CA bundle is injected into, and pairing the two here is
// what stops a kind from being served without being trusted.
type convertedKind struct {
	hub     client.Object
	crdName string
}

// convertedKinds is every kind that declares both v1alpha1 and v1alpha2, so every
// kind whose reads pass through the conversion webhook. Adding a kind to the
// migration means adding it here.
var convertedKinds = []convertedKind{
	{hub: &v1alpha2.ControlPlane{}, crdName: "controlplanes.storage.simplyblock.io"},
	{hub: &v1alpha2.StorageBackup{}, crdName: "storagebackups.storage.simplyblock.io"},
	{hub: &v1alpha2.StorageClusterOps{}, crdName: "storageclusterops.storage.simplyblock.io"},
	{hub: &v1alpha2.StorageNodeOps{}, crdName: "storagenodeops.storage.simplyblock.io"},
	{hub: &v1alpha2.StoragePool{}, crdName: "storagepools.storage.simplyblock.io"},
}

// ConvertedKindCRDNames returns the CRD names of every converted kind, for the
// CA-bundle injection to target.
func ConvertedKindCRDNames() []string {
	names := make([]string, 0, len(convertedKinds))
	for _, k := range convertedKinds {
		names = append(names, k.crdName)
	}
	return names
}

// SetupConversionWebhooks registers the conversion webhook for every converted
// kind. The builder detects the hub-and-spoke implementation on the types
// themselves and serves them all from one /convert path, so the repeated call
// registers the path once and adds a kind to it each time.
func SetupConversionWebhooks(mgr ctrl.Manager) error {
	for _, k := range convertedKinds {
		if err := builder.WebhookManagedBy(mgr, k.hub).Complete(); err != nil {
			return fmt.Errorf("register conversion webhook for %s: %w", k.crdName, err)
		}
	}
	return nil
}
