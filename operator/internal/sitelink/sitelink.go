// Package sitelink is the operator's end of the site link between the two sites
// of a stretched cluster (design-stretched-sync-replication-ramen.md §7): the
// elected operator links to its site's fleet agent, which passes it on to the relay
// on the hub, and through it reaches the other site's operator.
//
// What crosses the link is ordinary gRPC. This end serves gRPC health to the
// other site, which is what the other site's probe calls, and gives the
// StorageCluster controller ProbePeer, which reports whether the other site's
// operator answers. The services the later phases run over the link register on
// the same server.
package sitelink

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/link/sitev1"
)

const (
	// caConfigMap and caKey are where the site's fleet agent publishes the CA
	// its serving certificate verifies against.
	caConfigMap = "simplyblock-site-link-ca"
	caKey       = "ca.crt"
	// caPoll is how often the operator looks for a CA the agent has not
	// published yet.
	caPoll = 10 * time.Second
)

// Link is the operator's end of the site link. It runs on the elected replica
// only, since the other site addresses this site's operator as whichever one
// is linked.
type Link struct {
	// Pod and PodUID identify this replica to its site's agent.
	Pod    string
	PodUID string

	// Address is the site agent's Service, host:port.
	Address string
	// CANamespace is the agent's namespace, where it publishes its CA.
	CANamespace string
	// Kube reads that CA.
	Kube kubernetes.Interface

	// Dial overrides the TLS dialer built from Address and the published CA.
	// For tests.
	Dial link.Dialer
	// Token produces the projected ServiceAccount token the agent verifies.
	Token link.TokenSource
	// MinBackoff overrides the link's reconnect backoff. For tests.
	MinBackoff time.Duration
	// Logger receives the link's lifecycle. Nil means slog.Default.
	Logger *slog.Logger
	// Register adds the services this operator serves to the other site, such
	// as SwitchoverService, beside the health check its probe calls.
	Register func(grpc.ServiceRegistrar)

	mu    sync.RWMutex
	agent *link.Agent
}

var _ manager.LeaderElectionRunnable = (*Link)(nil)

// NeedLeaderElection keeps the link to the elected replica.
func (l *Link) NeedLeaderElection() bool { return true }

// Start links to the site's agent and serves the other site until ctx ends.
func (l *Link) Start(ctx context.Context) error {
	log := l.Logger
	if log == nil {
		log = slog.Default()
	}
	dial := l.Dial
	if dial == nil {
		var err error
		if dial, err = l.tlsDialer(ctx); err != nil {
			return err
		}
	}

	acceptor := link.NewAcceptor()
	srv := grpc.NewServer()
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, hs)
	if l.Register != nil {
		l.Register(srv)
	}
	go func() { _ = srv.Serve(acceptor) }()
	defer srv.Stop()

	agent, err := link.NewAgent(link.AgentConfig{
		Dial:        dial,
		ID:          link.OperatorPeer(l.Pod),
		InstanceUID: l.PodUID,
		Token:       l.Token,
		Register:    acceptor.Register,
		MinBackoff:  l.MinBackoff,
		Logger:      log,
	})
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.agent = agent
	l.mu.Unlock()

	log.Info("site link: linking to the site's agent", "address", l.Address, "pod", l.Pod)
	if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// Linked reports whether this replica is linked to its site's agent.
func (l *Link) Linked() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.agent != nil && l.agent.Linked()
}

// ProbePeer asks the operator of site, the other site's managed cluster, to
// answer over the site link, and returns why it did not.
func (l *Link) ProbePeer(ctx context.Context, site string) error {
	l.mu.RLock()
	agent := l.agent
	l.mu.RUnlock()
	if agent == nil || !agent.Linked() {
		return fmt.Errorf("site link: not linked to this site's agent: %w", link.ErrNoSession)
	}
	conn, err := link.RemoteConn(agent, link.Remote(site, link.PeerKindOperator, ""))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return fmt.Errorf("site link: the operator of site %s: %w", site, err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("site link: the operator of site %s answers %s", site, resp.GetStatus())
	}
	return nil
}

// tlsDialer dials the site's agent, trusting the CA it published. It waits for
// the CA rather than failing, since the agent may start after the operator.
func (l *Link) tlsDialer(ctx context.Context) (link.Dialer, error) {
	host, _, err := net.SplitHostPort(l.Address)
	if err != nil {
		return nil, fmt.Errorf("site link: address %q: %w", l.Address, err)
	}
	for {
		cm, err := l.Kube.CoreV1().ConfigMaps(l.CANamespace).Get(ctx, caConfigMap, metav1.GetOptions{})
		if err == nil && cm.Data[caKey] != "" {
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM([]byte(cm.Data[caKey])) {
				return nil, fmt.Errorf("site link: %s/%s holds no PEM certificate", l.CANamespace, caConfigMap)
			}
			return link.TLSDialer(l.Address, &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(caPoll):
		}
	}
}

// RefreshNode asks the operator of peerCluster, the other site's managed
// cluster, to refresh node's storage-node pod there, and reports whether a pod
// created after since is Ready (design-stretched-sync-replication-ramen.md
// §11.3). A node with no pod on that site has nothing to refresh, the same as a
// node of this site without one.
func (l *Link) RefreshNode(ctx context.Context, peerCluster, clusterUUID, node string, since time.Time) (bool, error) {
	l.mu.RLock()
	agent := l.agent
	l.mu.RUnlock()
	if agent == nil || !agent.Linked() {
		return false, fmt.Errorf("site link: not linked to this site's agent: %w", link.ErrNoSession)
	}
	conn, err := link.RemoteConn(agent, link.Remote(peerCluster, link.PeerKindOperator, ""))
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	resp, err := sitev1.NewSiteServiceClient(conn).RefreshNode(ctx, &sitev1.RefreshNodeRequest{
		ClusterUuid: clusterUUID, Node: node, SinceUnix: since.Unix(),
	})
	if err != nil {
		return false, fmt.Errorf("site link: the operator of site %s: %w", peerCluster, err)
	}
	switch resp.GetState() {
	case sitev1.RefreshNodeResponse_STATE_REFRESHED, sitev1.RefreshNodeResponse_STATE_NOT_FOUND:
		return true, nil
	default:
		return false, nil
	}
}
