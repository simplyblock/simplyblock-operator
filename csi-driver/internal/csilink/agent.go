// Package csilink runs the CSI driver's end of the link to the operator.
//
// The driver dials the operator and holds the connection open, and the operator
// issues its RPCs back down it. That direction is deliberate: nothing has to
// listen on a node, so the deployment needs no per-node ingress, no address
// discovery, and no NetworkPolicy beyond a pod reaching a Service.
//
// A dropped link is not an error to report upward but a reason to reconnect,
// which [Start] does until its context ends. The driver keeps working while
// unlinked: the link carries what the operator asks of this node, not the CSI
// operations kubelet asks of it.
package csilink

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/link"
)

// Config configures the driver's agent. HubAddress, ID and TokenFile are
// required.
type Config struct {
	// HubAddress is the operator's link endpoint, `host:port`.
	HubAddress string

	// CAFile is the bundle that signs the operator's serving certificate.
	// Empty, or naming a file that is not there, dials plaintext -- which is
	// what a cluster with no certificate provisioned for the link gets, and is
	// why the CA is mounted from an optional ConfigMap.
	CAFile string

	// ServerName overrides the name verified against that certificate. Needed
	// when the address dialed is not the name the certificate carries.
	ServerName string

	// TokenFile is the projected ServiceAccount token presented to the
	// operator. It is re-read on every attempt, because kubelet rewrites it
	// well before expiry and a cached copy would start failing to reconnect
	// hours later, during an outage, which is when reconnecting matters.
	TokenFile string

	// ID is the identity this peer asks to register as. The operator verifies
	// it against the token and refuses a link that disagrees, so it has to
	// come from the downward API rather than from configuration.
	ID link.PeerID

	// InstanceUID distinguishes this process lifetime from the previous one
	// (the pod UID), letting the operator supersede a session an earlier
	// instance left half-open.
	InstanceUID string

	// Register adds the services this peer answers, and Capabilities names
	// them in the handshake so the operator can tell what it can call.
	Register     func(grpc.ServiceRegistrar)
	Capabilities []string
}

// Start dials the operator and keeps the link up until ctx ends.
//
// It returns once the agent is running, not once it is linked: the operator may
// be restarting or mid-leader-election, and the agent will keep retrying with
// backoff. Callers that need to know use [link.Agent.Linked].
func Start(ctx context.Context, cfg Config) (*link.Agent, error) {
	if cfg.HubAddress == "" {
		return nil, fmt.Errorf("csi link: no hub address")
	}
	if cfg.TokenFile == "" {
		return nil, fmt.Errorf("csi link: no token file")
	}

	dial, err := dialer(cfg)
	if err != nil {
		return nil, err
	}

	agent, err := link.NewAgent(link.AgentConfig{
		Dial:         dial,
		ID:           cfg.ID,
		InstanceUID:  cfg.InstanceUID,
		Token:        link.TokenFile(cfg.TokenFile),
		Register:     cfg.Register,
		Capabilities: cfg.Capabilities,
	})
	if err != nil {
		return nil, fmt.Errorf("csi link: %w", err)
	}

	go func() {
		klog.Infof("CSI link: connecting to %s as %s", cfg.HubAddress, cfg.ID)
		if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
			klog.Errorf("CSI link: agent stopped: %v", err)
		}
	}()
	return agent, nil
}

// dialer verifies the operator when a CA bundle is there, and dials plaintext
// when there is none.
//
// Absent is the ordinary case rather than an error: the bundle comes from a
// ConfigMap mounted optional, so a cluster that provisioned no certificate for
// the link has no file here and the operator is serving plaintext to match. A
// bundle that exists and cannot be parsed is an error, because somebody
// provisioned one and it is broken.
func dialer(cfg Config) (link.Dialer, error) {
	if cfg.CAFile == "" {
		return link.InsecureDialer(cfg.HubAddress), nil
	}
	pem, err := os.ReadFile(cfg.CAFile)
	if errors.Is(err, os.ErrNotExist) {
		klog.Infof("CSI link: no CA bundle at %s, dialing %s without TLS", cfg.CAFile, cfg.HubAddress)
		return link.InsecureDialer(cfg.HubAddress), nil
	}
	if err != nil {
		return nil, fmt.Errorf("csi link: reading CA bundle %s: %w", cfg.CAFile, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("csi link: CA bundle %s has no usable certificates", cfg.CAFile)
	}
	return link.TLSDialer(cfg.HubAddress, &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: cfg.ServerName,
		RootCAs:    pool,
	}), nil
}
