package webhook

import (
	"context"
	"os"
	"path/filepath"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// mountedCertPollInterval is how often mountedCertWaiter looks for the files.
// The kubelet projects a Secret volume within a sync period, so the first
// look usually succeeds and the interval only matters on a slow node.
const mountedCertPollInterval = 2 * time.Second

// mountedCertWaiter closes ready once a serving certificate that someone else
// provisions is on disk in certDir. It runs under SB_TLS_PROVIDER=olm, where
// OLM generates the certificate, mounts it, and injects its CA, so the operator
// only has to know when the webhook server can start. Renewal needs nothing
// either: controller-runtime's certwatcher reloads the files when OLM rotates
// them.
type mountedCertWaiter struct {
	certDir  string
	ready    chan struct{}
	interval time.Duration
}

// NeedLeaderElection is false: every replica serves webhooks, so every replica
// has to start its webhook server, not only the leader.
func (w *mountedCertWaiter) NeedLeaderElection() bool { return false }

func (w *mountedCertWaiter) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("webhook-cert").WithValues("certDir", w.certDir, "provider", "olm")
	interval := w.interval
	if interval == 0 {
		interval = mountedCertPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if certFilesPresent(w.certDir) {
			log.Info("serving certificate is mounted")
			close(w.ready)
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// certFilesPresent reports whether certDir holds a non-empty key pair under the
// names the webhook server reads.
func certFilesPresent(certDir string) bool {
	for _, name := range []string{"tls.crt", "tls.key"} {
		info, err := os.Stat(filepath.Join(certDir, name))
		if err != nil || info.Size() == 0 {
			return false
		}
	}
	return true
}
