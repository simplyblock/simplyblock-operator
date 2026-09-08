// One call that puts the aggregated metrics API into a running operator:
// the PersistentVolume index the join needs, the serving certificate, and the
// server itself, in the order they depend on each other.
//
// It lives here rather than in cmd/main.go because the ordering is a property of
// this package and not of the process. The listener reads its key pair when it
// is constructed, so the server cannot be built until the rotator has written
// one; and the rotator only finishes after the manager is running, because it is
// a Runnable itself. So the server is added to the manager after it has started,
// which controller-runtime allows and which is exactly what the webhook
// registration next door does for the same reason.

package metricsapi

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/prometheus"
)

// Install registers everything the aggregated metrics API needs on mgr. It
// returns once the synchronous parts are done; the server itself is added to the
// manager later, from a goroutine that waits for the serving certificate.
//
// A failure after that point cannot be returned to the caller, because the
// caller has gone. It is logged and the API stays absent, which degrades the
// operator to what it did before this existed rather than taking it down: no
// reconciler depends on this server.
func Install(
	mgr ctrl.Manager,
	namespace string,
	port int,
	volumes VolumeSource,
	prometheusURL string,
) error {
	// The join reads PersistentVolumes by CSI volume handle. The key function is
	// atlas-lib's, shared with the CSI driver's client-go indexer, so both
	// resolve a handle the same way and only simplyblock-provisioned volumes are
	// keyed at all.
	err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.PersistentVolume{},
		kube.IndexPVByVolumeHandle, func(object client.Object) []string {
			pv, ok := object.(*corev1.PersistentVolume)
			if !ok {
				return nil
			}
			return kube.VolumeHandleKeys(pv)
		})
	if err != nil {
		return fmt.Errorf("metricsapi: index persistent volumes by volume handle: %w", err)
	}

	ready, err := SetupCertificate(mgr, namespace, CertDir)
	if err != nil {
		return fmt.Errorf("metricsapi: set up the serving certificate: %w", err)
	}

	log := mgr.GetLogger().WithName("metricsapi")

	// The sampled half of a reading. A logical volume's capacity block is all
	// zeros in the control plane's own DTO, so these numbers exist only in the
	// metrics the same service exports.
	//
	// An unconfigured or unbuildable endpoint is not fatal: the readings then
	// carry each volume's provisioned size and nothing measured. Serving the
	// identities and sizes beats serving nothing over a dependency this API can
	// answer partially without.
	var capacity CapacitySource
	if prometheusURL == "" {
		log.Info("no Prometheus endpoint configured; capacity samples will be absent")
	} else if provider, err := prometheus.New(prometheusURL); err != nil {
		log.Error(err, "capacity samples will be absent", "prometheusURL", prometheusURL)
	} else {
		capacity = provider
	}
	go func() {
		<-ready
		server, err := NewServer(
			Options{BindPort: port, CertDir: CertDir}, volumes, mgr.GetCache(), capacity, log,
		)
		if err != nil {
			log.Error(err, "the aggregated metrics API will not be served")
			return
		}
		if err := mgr.Add(server); err != nil {
			log.Error(err, "unable to add the aggregated metrics API server to the manager")
		}
	}()
	return nil
}
