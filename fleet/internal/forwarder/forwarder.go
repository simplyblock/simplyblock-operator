// Package forwarder is a site's end of the site link: the process the fleet
// add-on runs on every member, which links to the relay on the hub as its site
// and is a hub to its own site's operator and CSI controller
// (design-stretched-sync-replication-ramen.md §7.3).
//
// It holds no service logic and no control-plane credentials. Everything it does
// is atlas-lib's link.Forwarder; this package is the wiring a Kubernetes member
// needs around it: the two API servers it authenticates against, the hub token
// it links with, the relay endpoint it reads, and the serving certificate its
// site's peers verify it by.
package forwarder

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/simplyblock/atlas/link"

	"github.com/simplyblock/fleet/internal/sitelink"
)

// endpointPoll is how often the agent looks for the relay endpoint the hub has
// not published yet.
const endpointPoll = 10 * time.Second

// Config configures one site's agent.
type Config struct {
	// Site is the member's name, which is its site and its namespace on the hub.
	Site string
	// Namespace is the agent's namespace on the site, where it publishes the
	// CA its site's peers trust it by.
	Namespace string
	// InstanceUID is the agent's pod UID.
	InstanceUID string
	// ListenAddr is where the agent's local hub listens for its site's peers.
	ListenAddr string
	// DNSNames are the names its site's peers reach it by: its Service's.
	DNSNames []string
	// PeerServiceAccounts restricts which of its site's ServiceAccounts may
	// link as which kind, as namespace/name.
	PeerServiceAccounts map[link.PeerKind][]string
	// Hub is a client of the hub's API server, with the add-on's credentials.
	Hub kubernetes.Interface
	// Local is a client of the site's own API server.
	Local kubernetes.Interface
	// Logger receives the link's lifecycle. Nil means slog.Default.
	Logger *slog.Logger
}

// Run serves the site's peers and links to the relay until ctx ends.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	log := cfg.Logger

	cert, caPEM, err := sitelink.SelfSigned(cfg.DNSNames, nil)
	if err != nil {
		return err
	}
	if err := publishLocalCA(ctx, cfg.Local, cfg.Namespace, caPEM); err != nil {
		return err
	}

	fwd := link.NewForwarder(link.ForwarderConfig{Site: cfg.Site, Logger: log})

	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("forwarder: listening on %s: %w", cfg.ListenAddr, err)
	}
	local, err := link.NewHub(link.HubConfig{
		Listener: tls.NewListener(lis, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}),
		Auth: &link.KubeAuthenticator{
			Client:          cfg.Local,
			Audiences:       []string{sitelink.Audience},
			ServiceAccounts: cfg.PeerServiceAccounts,
		},
		Register: fwd.RegisterLocal,
		Logger:   log,
	})
	if err != nil {
		_ = lis.Close()
		return err
	}
	fwd.SetLocal(local.Registry())

	errc := make(chan error, 2)
	go func() { errc <- local.Serve(ctx) }()

	ep, err := awaitEndpoint(ctx, cfg.Hub, cfg.Site, endpointPoll)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ep.CA) {
		return fmt.Errorf("forwarder: the relay's CA from the hub is not a PEM certificate")
	}
	host, _, err := net.SplitHostPort(ep.Address)
	if err != nil {
		return fmt.Errorf("forwarder: relay address %q: %w", ep.Address, err)
	}
	upstream, err := link.NewAgent(link.AgentConfig{
		Dial:        link.TLSDialer(ep.Address, &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}),
		ID:          link.SitePeer(cfg.Site),
		InstanceUID: cfg.InstanceUID,
		Token:       sitelink.HubToken(cfg.Hub, cfg.Site),
		Register:    fwd.RegisterUpstream,
		Logger:      log,
	})
	if err != nil {
		return err
	}
	fwd.SetUpstream(upstream)
	go func() { errc <- upstream.Run(ctx) }()

	log.Info("site link: forwarding", "site", cfg.Site, "relay", ep.Address, "listen", cfg.ListenAddr)
	err = <-errc
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// publishLocalCA writes the CA the agent's serving certificate verifies against
// to the ConfigMap its site's peers read, replacing what an earlier instance
// published.
func publishLocalCA(ctx context.Context, client kubernetes.Interface, namespace string, caPEM []byte) error {
	want := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: sitelink.LocalCAConfigMap, Namespace: namespace},
		Data:       map[string]string{sitelink.CAKey: string(caPEM)},
	}
	configMaps := client.CoreV1().ConfigMaps(namespace)
	have, err := configMaps.Get(ctx, sitelink.LocalCAConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = configMaps.Create(ctx, want, metav1.CreateOptions{})
		return wrapPublish(namespace, err)
	}
	if err != nil {
		return wrapPublish(namespace, err)
	}
	have.Data = want.Data
	_, err = configMaps.Update(ctx, have, metav1.UpdateOptions{})
	return wrapPublish(namespace, err)
}

func wrapPublish(namespace string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("forwarder: publishing %s/%s: %w", namespace, sitelink.LocalCAConfigMap, err)
}

// awaitEndpoint returns the relay endpoint once the hub has published it in the
// site's namespace, checking every poll, or ctx's error if it ends first.
func awaitEndpoint(ctx context.Context, hub kubernetes.Interface, site string, poll time.Duration) (sitelink.Endpoint, error) {
	for {
		ep, err := sitelink.ReadEndpoint(ctx, hub, site)
		if err == nil {
			return ep, nil
		}
		select {
		case <-ctx.Done():
			return sitelink.Endpoint{}, errors.Join(ctx.Err(), err)
		case <-time.After(poll):
		}
	}
}
