// Package relay is the hub's half of the site link
// (design-stretched-sync-replication-ramen.md §7.2): the relay server that every
// site's agent links to, the certificate it serves, and the objects each member
// needs in its namespace on the hub to reach it.
//
// The relay itself is atlas-lib's link.Relay on a link.Hub. What this package
// adds is what running it on an Open Cluster Management hub takes: a
// ServiceAccount per member, which the member's agent requests its link token
// for and which the relay's authenticator knows the site by; a ConfigMap per
// member that names the relay; and a serving certificate that outlives a
// restart, since every agent trusts the CA it was given at its own start.
package relay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/simplyblock/atlas/link"

	"github.com/simplyblock/fleet/internal/sitelink"
)

// tlsSecret holds the relay's certificate and key on the hub.
const tlsSecret = "simplyblock-site-link-tls"

// MemberReconciler gives each member of the fleet, in its namespace on the hub,
// the ServiceAccount its agent requests a link token for and the ConfigMap that
// names the relay.
type MemberReconciler struct {
	client.Client
	// Address is where the members reach the relay, as host:port.
	Address string
	// CA is the PEM certificate the relay's serving certificate verifies against.
	CA []byte
}

// +kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=managedclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts;configmaps,verbs=get;list;watch;create;update

// Reconcile ensures one member's site-link objects. A member whose namespace is
// gone has nothing to write into; OCM removes the namespace with the member.
func (r *MemberReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cluster clusterv1.ManagedCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	namespace := cluster.Name

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sitelink.ServiceAccount, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error { return nil }); err != nil {
		return ctrl.Result{}, fmt.Errorf("site link ServiceAccount in %s: %w", namespace, err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: sitelink.EndpointConfigMap, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{sitelink.AddressKey: r.Address, sitelink.CAKey: string(r.CA)}
		return nil
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("site link endpoint in %s: %w", namespace, err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager reconciles every ManagedCluster.
func (r *MemberReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&clusterv1.ManagedCluster{}).Named("sitelink-member").Complete(r)
}

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create

// LoadOrCreateTLS returns the relay's serving certificate and its CA's PEM,
// from the Secret in namespace when there is one and from a new self-signed
// certificate for hosts, stored there, when there is not.
func LoadOrCreateTLS(ctx context.Context, c client.Client, namespace string, hosts []string) (tls.Certificate, []byte, error) {
	var secret corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: tlsSecret}, &secret)
	if err == nil {
		cert, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
		if err != nil {
			return tls.Certificate{}, nil, fmt.Errorf("relay: the certificate in %s/%s: %w", namespace, tlsSecret, err)
		}
		return cert, secret.Data[corev1.TLSCertKey], nil
	}
	if !apierrors.IsNotFound(err) {
		return tls.Certificate{}, nil, err
	}

	var dnsNames, ips []string
	for _, host := range hosts {
		if net.ParseIP(host) != nil {
			ips = append(ips, host)
		} else {
			dnsNames = append(dnsNames, host)
		}
	}
	cert, caPEM, err := sitelink.SelfSigned(dnsNames, ips)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	secret = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tlsSecret, Namespace: namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       caPEM,
			corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		},
	}
	if err := c.Create(ctx, &secret); err != nil {
		// Two replicas starting together both make one; the second to write
		// takes the first's, so both serve the same certificate.
		if apierrors.IsAlreadyExists(err) {
			return LoadOrCreateTLS(ctx, c, namespace, hosts)
		}
		return tls.Certificate{}, nil, err
	}
	return cert, caPEM, nil
}

// Server is the relay as a manager runnable. It runs on the elected replica
// only, and its hub accepts only while that replica holds the lease.
type Server struct {
	// ListenAddr is where the relay listens.
	ListenAddr string
	// Cert is the relay's serving certificate.
	Cert tls.Certificate
	// Hub is a client of the hub's API server, for the TokenReviews that tell
	// a site's agent from anything else.
	Hub kubernetes.Interface
	// Elected is closed once this replica leads.
	Elected <-chan struct{}
	// Logger receives the relay's lifecycle.
	Logger *slog.Logger
}

var _ manager.LeaderElectionRunnable = (*Server)(nil)

// NeedLeaderElection keeps the relay to the elected replica: a site's agent
// links to whichever replica its Service reaches, and only the leader's is up.
func (s *Server) NeedLeaderElection() bool { return true }

// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create

// Start serves the relay until ctx ends.
func (s *Server) Start(ctx context.Context) error {
	lis, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return fmt.Errorf("relay: listening on %s: %w", s.ListenAddr, err)
	}
	registry := link.NewRegistry()
	relay := link.NewRelay(registry, s.Logger)
	hub, err := link.NewHub(link.HubConfig{
		Listener: tls.NewListener(lis, &tls.Config{Certificates: []tls.Certificate{s.Cert}, MinVersion: tls.VersionTLS12}),
		Auth: &link.SiteAuthenticator{
			Client:         s.Hub,
			ServiceAccount: sitelink.ServiceAccount,
			Audiences:      []string{sitelink.Audience},
		},
		Accepting: func() bool {
			select {
			case <-s.Elected:
				return true
			default:
				return false
			}
		},
		Registry: registry,
		Register: relay.Register,
		Logger:   s.Logger,
	})
	if err != nil {
		_ = lis.Close()
		return err
	}
	if err := hub.Serve(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
