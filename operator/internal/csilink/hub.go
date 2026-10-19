// Package csilink runs the operator's end of the link the CSI driver opens.
//
// The CSI node and controller pods dial the operator and hold the connection;
// the operator issues its RPCs back down it. Nothing listens on a node, so no
// per-node ingress or discovery is needed — see the atlas link package for why
// the connection runs backward and how gRPC still works over it.
//
// Setup adds the hub to the manager and hands back the registry the reconcilers
// read. A peer that is not currently linked is normal, not exceptional: expect
// link.ErrNoSession during rollouts and leadership changes, and requeue on it.
package csilink

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
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
	// BindAddress is where peers dial, for example, ":9500."
	BindAddress string

	// CertFile and KeyFile are the hub's serving certificate. Set, the hub
	// serves TLS and nothing else, waiting for the files to appear if they are
	// not there yet. Unset, it serves plaintext, which is what a deployment
	// with TLS off configures.
	//
	// Peers authenticate with bearer tokens either way, so plaintext publishes
	// them to anything on the path.
	CertFile string
	KeyFile  string

	// ClientCAFile, when set, turns on mutual TLS: a peer must present a client
	// certificate this bundle verifies, or the handshake fails before its token
	// is read. It needs CertFile and KeyFile.
	ClientCAFile string

	// ClientNames are the common names a client certificate may carry. The CA
	// behind the link issues certificates to other components too, and only
	// the CSI identities have any business on it. Empty admits any name the CA
	// signed.
	ClientNames []string

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

	// MDSServiceAccount names the pNFS metadata server pod's ServiceAccount,
	// unqualified. Empty closes the MDS kind to every peer.
	MDSServiceAccount string
}

// The hub needs to verify peer tokens, and — on clusters whose API server does
// not report the node on a TokenReview — to read the pod a token is bound to in
// order to learn which node it runs on.
//
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get

// serviceAccounts maps each peer kind to the one ServiceAccount that may link
// as it. A kind with no entry is refused, so the metadata server's kind stays
// closed until its ServiceAccount is configured.
func serviceAccounts(cfg Config) map[link.PeerKind][]string {
	accounts := map[link.PeerKind][]string{
		link.PeerKindNode:       {cfg.Namespace + "/" + cfg.NodeServiceAccount},
		link.PeerKindController: {cfg.Namespace + "/" + cfg.ControllerServiceAccount},
	}
	if cfg.MDSServiceAccount != "" {
		accounts[link.PeerKindMDS] = []string{cfg.Namespace + "/" + cfg.MDSServiceAccount}
	}
	return accounts
}

// Setup starts the hub under mgr and returns the registry of linked peers.
//
// The hub runs on every replica rather than only the leader, because the
// Service may route a peer to any of them; the ones that are not leading refuse
// the handshake so the peer redials and lands on the one that is. Reconcilers
// take the returned registry.
func Setup(mgr ctrl.Manager, cfg Config) (*link.Registry, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("csi link: kubernetes client: %w", err)
	}

	listener, err := listen(cfg)
	if err != nil {
		return nil, err
	}

	hub, err := link.NewHub(link.HubConfig{
		Listener: listener,
		Auth: &link.KubeAuthenticator{
			Client:          clientset,
			Audiences:       cfg.Audiences,
			ServiceAccounts: serviceAccounts(cfg),
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

// listen serves TLS when a certificate is configured, and plaintext when none
// is.
//
// A configured certificate that cannot be read yet is not a reason to fail or
// to fall back. It is issued in the cluster and mounted from an optional
// Secret, so the operator normally starts before it exists. The listener
// serves TLS from the start, and handshakes fail until the files land. Serving
// plaintext in that window would hand every peer's token to the network.
func listen(cfg Config) (net.Listener, error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		if cfg.ClientCAFile != "" {
			return nil, errors.New("csi link: mutual TLS needs a serving certificate")
		}
		logf.Log.WithName("csi-link").Info(
			"serving the CSI link without TLS: no certificate is configured, "+
				"so peers' ServiceAccount tokens travel in the clear",
			"address", cfg.BindAddress)
		listener, err := net.Listen("tcp", cfg.BindAddress)
		if err != nil {
			return nil, fmt.Errorf("csi link: listen on %s: %w", cfg.BindAddress, err)
		}
		return listener, nil
	}

	certs := &certReloader{certFile: cfg.CertFile, keyFile: cfg.KeyFile}
	if _, err := certs.load(); err != nil {
		logf.Log.WithName("csi-link").Info(
			"the CSI link serving certificate is not readable yet; handshakes fail until it is",
			"certFile", cfg.CertFile, "error", err.Error())
	}
	serving := &tls.Config{GetCertificate: certs.get, MinVersion: tls.VersionTLS13}
	if cfg.ClientCAFile != "" {
		// Built per handshake so that a rotated CA bundle is picked up the
		// same way a rotated serving certificate is. Links are long-lived,
		// so handshakes are rare and reading a small file on each is cheap.
		serving.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return mutualConfig(cfg, certs)
		}
	}
	listener, err := tls.Listen("tcp", cfg.BindAddress, serving)
	if err != nil {
		return nil, fmt.Errorf("csi link: listen on %s: %w", cfg.BindAddress, err)
	}
	return listener, nil
}

// mutualConfig is the serving configuration for one handshake under mutual
// TLS: the client certificate is required, verified against ClientCAFile, and
// must carry one of ClientNames.
func mutualConfig(cfg Config, certs *certReloader) (*tls.Config, error) {
	bundle, err := os.ReadFile(cfg.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("csi link: reading client CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, fmt.Errorf("csi link: client CA bundle %s has no usable certificates", cfg.ClientCAFile)
	}
	return &tls.Config{
		GetCertificate:   certs.get,
		MinVersion:       tls.VersionTLS13,
		ClientCAs:        pool,
		ClientAuth:       tls.RequireAndVerifyClientCert,
		VerifyConnection: clientNameIn(cfg.ClientNames),
	}, nil
}

// clientNameIn refuses a verified client certificate whose common name is not
// one of names. It runs after chain verification, so the name it reads is one
// the CA vouched for.
func clientNameIn(names []string) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if len(names) == 0 {
			return nil
		}
		if len(state.PeerCertificates) == 0 {
			return errors.New("csi link: no client certificate")
		}
		name := state.PeerCertificates[0].Subject.CommonName
		if !slices.Contains(names, name) {
			return fmt.Errorf("csi link: client certificate %q is not issued to a CSI component", name)
		}
		return nil
	}
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

	// Both halves decide staleness, not the certificate alone: a key rewritten
	// while the certificate keeps its mtime still makes the loaded pair stale,
	// and watching only the certificate would go on serving the old pair for
	// the life of the process.
	changed, err := c.modTimeOfPair()
	if err != nil {
		if c.cert != nil {
			return c.cert, nil // keep serving what we have
		}
		return nil, err
	}
	c.lastLoad = time.Now()
	if c.cert != nil && changed.Equal(c.modTime) {
		return c.cert, nil
	}

	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		if c.cert != nil {
			// A half-written rotation is transient; the old cert still works.
			// modTime is left alone so the next interval retries the load.
			return c.cert, nil
		}
		return nil, err
	}
	c.cert, c.modTime = &cert, changed
	return c.cert, nil
}

// modTimeOfPair is the later of the two files' modification times, which is the
// one instant that moves whenever either half is rewritten. It fails unless
// both are readable: half a keypair is not something to serve.
func (c *certReloader) modTimeOfPair() (time.Time, error) {
	certInfo, err := os.Stat(c.certFile)
	if err != nil {
		return time.Time{}, err
	}
	keyInfo, err := os.Stat(c.keyFile)
	if err != nil {
		return time.Time{}, err
	}
	if keyInfo.ModTime().After(certInfo.ModTime()) {
		return keyInfo.ModTime(), nil
	}
	return certInfo.ModTime(), nil
}
