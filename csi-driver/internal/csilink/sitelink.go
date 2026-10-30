package csilink

// The CSI controller's end of a stretched cluster's site link
// (design-stretched-sync-replication-ramen.md §7.3): beside its link to the
// operator, the controller links to its site's fleet agent, so the other site's
// operator can reach it through the relay on the hub. It answers gRPC health
// there today; the services the later phases call it for register beside it.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/link"
)

const (
	// siteLinkCAConfigMap is where the site's fleet agent publishes the CA its
	// serving certificate verifies against, in the agent's namespace.
	siteLinkCAConfigMap = "simplyblock-site-link-ca"
	// siteLinkCAPoll is how often the controller looks for a CA the agent has
	// not published yet.
	siteLinkCAPoll = 10 * time.Second
)

// SiteLinkConfig configures the controller's site link.
type SiteLinkConfig struct {
	// Address is the site's fleet agent, host:port.
	Address string
	// AgentNamespace is where the agent publishes its CA.
	AgentNamespace string
	// TokenFile is the projected token, with the site link's audience.
	TokenFile string
	// ID and InstanceUID identify this controller pod.
	ID          link.PeerID
	InstanceUID string
}

// StartSiteLink links the controller to its site's fleet agent in the
// background until ctx ends, once the agent has published its CA.
func StartSiteLink(ctx context.Context, kube kubernetes.Interface, cfg SiteLinkConfig) {
	go func() {
		dial, err := siteLinkDialer(ctx, kube, cfg.AgentNamespace, cfg.Address)
		if err != nil {
			if ctx.Err() == nil {
				klog.Errorf("site link: %v", err)
			}
			return
		}
		acceptor := link.NewAcceptor()
		srv := grpc.NewServer()
		hs := health.NewServer()
		hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
		healthpb.RegisterHealthServer(srv, hs)
		go func() { _ = srv.Serve(acceptor) }()
		defer srv.Stop()

		agent, err := link.NewAgent(link.AgentConfig{
			Dial:        dial,
			ID:          cfg.ID,
			InstanceUID: cfg.InstanceUID,
			Token:       link.TokenFile(cfg.TokenFile),
			Register:    acceptor.Register,
		})
		if err != nil {
			klog.Errorf("site link: %v", err)
			return
		}
		klog.Infof("site link: connecting to %s as %s", cfg.Address, cfg.ID)
		if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
			klog.Errorf("site link: agent stopped: %v", err)
		}
	}()
}

// siteLinkDialer dials the site's agent, trusting the CA it published. Unlike
// the operator's link, there is no plaintext fallback: the agent always serves
// TLS, so a CA that is missing is waited for and one that is broken is an error.
func siteLinkDialer(ctx context.Context, kube kubernetes.Interface, namespace, address string) (link.Dialer, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("site link: address %q: %w", address, err)
	}
	for {
		cm, err := kube.CoreV1().ConfigMaps(namespace).Get(ctx, siteLinkCAConfigMap, metav1.GetOptions{})
		if err == nil && cm.Data["ca.crt"] != "" {
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM([]byte(cm.Data["ca.crt"])) {
				return nil, fmt.Errorf("site link: %s/%s holds no PEM certificate", namespace, siteLinkCAConfigMap)
			}
			return link.TLSDialer(address, &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(siteLinkCAPoll):
		}
	}
}
