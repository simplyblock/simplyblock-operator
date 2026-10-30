// Package sitelink holds what the relay on the hub and a site's agent share:
// the names they agree on, the hub token an agent links with, the relay
// endpoint it reads, and the self-signed certificates both ends serve
// (design-stretched-sync-replication-ramen.md §7.2, §7.3).
//
// The agent's credential for the relay is a token for the site link's
// ServiceAccount in the agent's own member namespace on the hub. The add-on's
// registration gives the agent a client certificate for the hub, which the
// relay cannot verify with a TokenReview, so the agent uses that certificate
// only to request the token, and the relay verifies the token. The member
// namespace is the site: only that member's add-on may request a token there.
package sitelink

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/simplyblock/atlas/link"
)

const (
	// ServiceAccount is the site link's ServiceAccount in each member's
	// namespace on the hub. The relay accepts tokens for it and no other.
	ServiceAccount = "simplyblock-site-link"
	// Audience is the audience of every site-link token, on the hub and on a
	// site, so a token issued for the link is useless anywhere else.
	Audience = "simplyblock-site-link"
	// EndpointConfigMap is the ConfigMap in each member's namespace on the hub
	// that names the relay: its address and the CA its certificate is signed by.
	EndpointConfigMap = "simplyblock-site-link"
	// LocalCAConfigMap is the ConfigMap in the agent's namespace on a site that
	// carries the CA of the agent's own serving certificate, for the site's
	// peers to trust.
	LocalCAConfigMap = "simplyblock-site-link-ca"
	// CAKey is the key under which either ConfigMap carries a PEM CA.
	CAKey = "ca.crt"
	// AddressKey is the key under which the endpoint ConfigMap carries the
	// relay's address, as host:port.
	AddressKey = "address"
)

// tokenLifetime is what a hub token is requested for. Renewal begins once less
// than a fifth of a token's lifetime remains, and never later than
// tokenMinMargin before it expires: a token presented with seconds to live can
// lapse during the relay's TokenReview.
const (
	tokenLifetime  = time.Hour
	tokenMinMargin = time.Minute
)

// Endpoint is where a site's agent reaches the relay.
type Endpoint struct {
	// Address is host:port.
	Address string
	// CA is the PEM certificate the relay's serving certificate verifies against.
	CA []byte
}

// HubToken requests tokens for the site link's ServiceAccount in namespace on
// the hub, and reuses each until less than a fifth of its lifetime, or less
// than a minute, remains.
func HubToken(hub kubernetes.Interface, namespace string) link.TokenSource {
	var (
		mu      sync.Mutex
		current string
		renewAt time.Time
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if current != "" && time.Now().Before(renewAt) {
			return current, nil
		}
		issued := time.Now()
		req, err := hub.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, ServiceAccount,
			&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
				Audiences:         []string{Audience},
				ExpirationSeconds: ptr.To(int64(tokenLifetime.Seconds())),
			}}, metav1.CreateOptions{})
		if err != nil {
			return "", fmt.Errorf("sitelink: requesting a token for %s/%s: %w", namespace, ServiceAccount, err)
		}
		expires := req.Status.ExpirationTimestamp.Time
		current = req.Status.Token
		renewAt = expires.Add(-max(expires.Sub(issued)/5, tokenMinMargin))
		return current, nil
	}
}

// ReadEndpoint reads the relay endpoint the hub published in a member's
// namespace. A missing ConfigMap, address, or CA is an error: the agent waits
// for the hub rather than dialing an address it guessed.
func ReadEndpoint(ctx context.Context, hub kubernetes.Interface, namespace string) (Endpoint, error) {
	cm, err := hub.CoreV1().ConfigMaps(namespace).Get(ctx, EndpointConfigMap, metav1.GetOptions{})
	if err != nil {
		return Endpoint{}, fmt.Errorf("sitelink: reading %s/%s on the hub: %w", namespace, EndpointConfigMap, err)
	}
	ep := Endpoint{Address: cm.Data[AddressKey], CA: []byte(cm.Data[CAKey])}
	if ep.Address == "" || len(ep.CA) == 0 {
		return Endpoint{}, fmt.Errorf("sitelink: %s/%s on the hub has no %s or no %s",
			namespace, EndpointConfigMap, AddressKey, CAKey)
	}
	return ep, nil
}

// SelfSigned makes a self-signed serving certificate for the given DNS names and
// IP addresses, and returns it with its PEM, which is the CA a client trusts it
// by. It is valid for ten years: rotating it means restarting the process that
// made it and republishing the PEM, which both ends of the link do on start.
func SelfSigned(dnsNames, ipAddresses []string) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("sitelink: generating a key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("sitelink: generating a serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "simplyblock-site-link"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
	}
	for _, ip := range ipAddresses {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			return tls.Certificate{}, nil, fmt.Errorf("sitelink: %q is not an IP address", ip)
		}
		template.IPAddresses = append(template.IPAddresses, parsed)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("sitelink: signing the certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
