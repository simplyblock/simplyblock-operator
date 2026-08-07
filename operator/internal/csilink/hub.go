// Package csilink runs the operator's end of the link the CSI driver opens.
//
// The CSI node and controller pods dial the operator and hold the connection;
// the operator issues its RPCs back down it. Nothing listens on a node, so no
// per-node ingress or discovery is needed — see the atlas link package for why
// the connection runs backwards and how gRPC still works over it.
//
// Setup adds the hub to the manager and hands back the registry the reconcilers
// read. A peer that is not currently linked is normal, not exceptional: expect
// link.ErrNoSession during rollouts and leadership changes, and requeue on it.
package csilink

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/link"
)

// Config configures the operator's hub.
type Config struct {
	// BindAddress is where peers dial, e.g. ":9500".
	BindAddress string

	// CertFile and KeyFile are the hub's serving certificate. Both are
	// required: peers authenticate with bearer tokens, which must not travel
	// in the clear.
	CertFile string
	KeyFile  string

	// Namespace is the operator's namespace, used to qualify the CSI
	// ServiceAccounts allowed to link.
	Namespace string

	// Audiences is the audience the peers' projected tokens must carry. A
	// token bound to no particular audience is one any component holding it
	// can replay here.
	Audiences []string

	// NodeServiceAccount and ControllerServiceAccount name the CSI
	// ServiceAccounts, unqualified. Each may register only as its own kind.
	NodeServiceAccount       string
	ControllerServiceAccount string
}

// The hub needs to verify peer tokens, and — on clusters whose API server does
// not report the node on a TokenReview — to read the pod a token is bound to in
// order to learn which node it runs on.
//
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get

// Setup starts the hub under mgr and returns the registry of linked peers.
//
// The hub runs on every replica rather than only the leader, because the
// Service may route a peer to any of them; the ones that are not leading refuse
// the handshake so the peer redials and lands on the one that is. Reconcilers
// take the returned registry.
func Setup(mgr ctrl.Manager, cfg Config) (*link.Registry, error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, fmt.Errorf("csi link: a serving certificate is required")
	}

	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("csi link: kubernetes client: %w", err)
	}

	certs := &certReloader{certFile: cfg.CertFile, keyFile: cfg.KeyFile}
	if _, err := certs.load(); err != nil {
		return nil, fmt.Errorf("csi link: serving certificate: %w", err)
	}

	listener, err := tls.Listen("tcp", cfg.BindAddress, &tls.Config{
		GetCertificate: certs.get,
		MinVersion:     tls.VersionTLS13,
	})
	if err != nil {
		return nil, fmt.Errorf("csi link: listen on %s: %w", cfg.BindAddress, err)
	}

	hub, err := link.NewHub(link.HubConfig{
		Listener: listener,
		Auth: &link.KubeAuthenticator{
			Client:    clientset,
			Audiences: cfg.Audiences,
			ServiceAccounts: map[link.PeerKind][]string{
				link.PeerKindNode:       {cfg.Namespace + "/" + cfg.NodeServiceAccount},
				link.PeerKindController: {cfg.Namespace + "/" + cfg.ControllerServiceAccount},
			},
		},
		// Only the replica doing the reconciling may hold peers. A follower
		// answers Unavailable, which the agent retries — landing, via the
		// Service, on whichever replica is leading.
		Accepting: func() bool {
			select {
			case <-mgr.Elected():
				return true
			default:
				return false
			}
		},
		Logger: slog.New(logr.ToSlogHandler(mgr.GetLogger().WithName("csi-link"))),
	})
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("csi link: %w", err)
	}

	if err := mgr.Add(&hubRunnable{hub: hub}); err != nil {
		_ = hub.Close()
		return nil, fmt.Errorf("csi link: add to manager: %w", err)
	}
	return hub.Registry(), nil
}

// hubRunnable adapts the hub to the manager's lifecycle.
type hubRunnable struct{ hub *link.Hub }

// NeedLeaderElection is false: the listener has to exist wherever the Service
// may route, and leadership is enforced by the hub's Accepting gate instead.
func (r *hubRunnable) NeedLeaderElection() bool { return false }

func (r *hubRunnable) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("csi-link")
	log.Info("serving CSI link", "address", r.hub.Addr())

	// Serve returns the context's error on a clean shutdown; a Runnable that
	// returns non-nil takes the manager down with it.
	err := r.hub.Serve(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// certReloader serves the hub's certificate, re-reading it when it changes on
// disk. Long-lived listeners outlive their certificates: cert-manager and the
// service-CA both rotate the secret in place, and a keypair loaded once at
// startup would go on being served until the operator happened to restart.
type certReloader struct {
	certFile string
	keyFile  string

	mu       sync.Mutex
	cert     *tls.Certificate
	modTime  time.Time
	lastLoad time.Time
}

// reloadInterval bounds how often the files are stat'ed, so a busy listener
// does not hit the filesystem on every handshake.
const reloadInterval = 30 * time.Second

func (c *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.load()
}

func (c *certReloader) load() (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cert != nil && time.Since(c.lastLoad) < reloadInterval {
		return c.cert, nil
	}

	info, err := os.Stat(c.certFile)
	if err != nil {
		if c.cert != nil {
			return c.cert, nil // keep serving what we have
		}
		return nil, err
	}
	c.lastLoad = time.Now()
	if c.cert != nil && info.ModTime().Equal(c.modTime) {
		return c.cert, nil
	}

	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		if c.cert != nil {
			// A half-written rotation is transient; the old cert still works.
			return c.cert, nil
		}
		return nil, err
	}
	c.cert, c.modTime = &cert, info.ModTime()
	return c.cert, nil
}