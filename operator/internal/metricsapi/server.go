// The aggregated API server: an HTTPS listener the kube-apiserver proxies
// /apis/metrics.simplyblock.io/... to, running inside the operator process as one
// more controller-runtime Runnable.
//
// It is in-process rather than a second Deployment because the data it serves is
// already here. The volume cache is fed by a control-plane stream the operator
// holds open anyway, and the claim correlation reads the manager's own
// Kubernetes cache; a separate binary would need both again, which is two more
// streams, a second cache, and a second thing to keep from going stale.
//
// It is not leader-elected. The kube-apiserver reaches it through a Service, so
// the request lands on whichever replica the Service picked, and a follower that
// declined to start would either refuse the connection or answer from an empty
// cache. Reads are safe to serve from every replica because nothing here writes
// anything.
//
// Authentication and authorization are delegated to the kube-apiserver, which is
// what makes an ordinary RoleBinding on this group's resources work: the caller's
// token is verified by TokenReview and their access by SubjectAccessReview, both
// against the cluster's own RBAC. That delegation is the reason for the two
// bindings in config/rbac (system:auth-delegator, and the reader on
// extension-apiserver-authentication in kube-system). Without them every
// request fails closed with a 401 that looks like a certificate problem.

package metricsapi

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/go-logr/logr"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	basecompatibility "k8s.io/component-base/compatibility"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metricsv1alpha1 "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha1"
)

// Default wiring of the listener. The port is the conventional one for an
// extension API server, and the certificate directory is the one the rotator in
// cert.go writes into.
const (
	// DefaultBindPort is the HTTPS port the aggregated API listens on.
	DefaultBindPort = 6443
	// CertDir is where the serving certificate is written and watched.
	CertDir = "/tmp/k8s-metrics-apiserver/serving-certs"
	// certPairName is the basename of the certificate pair in CertDir, matching
	// what cert-controller writes into a Secret it manages.
	certPairName = "tls"
	// operatorAPIVersion is what /version reports for this API surface. It
	// tracks the group's own maturity, not the operator build, so that a client
	// reading it learns something about the contract rather than about the
	// release train.
	operatorAPIVersion = "v1.0.0"
)

// Options configures the aggregated API server.
type Options struct {
	// BindPort is the HTTPS port to listen on; 0 selects DefaultBindPort.
	BindPort int
	// CertDir holds tls.crt and tls.key; "" selects CertDir.
	CertDir string
}

func (o *Options) withDefaults() {
	if o.BindPort == 0 {
		o.BindPort = DefaultBindPort
	}
	if o.CertDir == "" {
		o.CertDir = CertDir
	}
}

// Server is the aggregated API server as a manager.Runnable.
type Server struct {
	server *genericapiserver.GenericAPIServer
	log    logr.Logger
}

// NewServer builds the aggregated API server over the given volume cache and
// Kubernetes reader. The reader must be a cache with kube.IndexPVByVolumeHandle
// registered on PersistentVolumes.
//
// The serving certificate must already exist in opts.CertDir: the listener reads
// it at construction, so callers wait on the rotator's ready channel first (see
// SetupCertificate).
func NewServer(
	opts Options,
	volumes VolumeSource,
	reader client.Reader,
	capacity CapacitySource,
	log logr.Logger,
) (*Server, error) {
	opts.withDefaults()

	config := genericapiserver.NewRecommendedConfig(Codecs)
	// The server reports a version on /version and gates its own features on
	// one, and the config carries no default. It is the operator's version
	// rather than Kubernetes': this API's compatibility is ours to state.
	config.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString(operatorAPIVersion, "", "")

	secure := genericoptions.NewSecureServingOptions().WithLoopback()
	secure.BindPort = opts.BindPort
	secure.ServerCert.CertDirectory = opts.CertDir
	secure.ServerCert.PairName = certPairName
	secure.ServerCert.CertKey = genericoptions.CertKey{
		CertFile: filepath.Join(opts.CertDir, certPairName+".crt"),
		KeyFile:  filepath.Join(opts.CertDir, certPairName+".key"),
	}
	if err := secure.ApplyTo(&config.SecureServing, &config.LoopbackClientConfig); err != nil {
		return nil, fmt.Errorf("metricsapi: configure secure serving: %w", err)
	}

	// RemoteKubeConfigFileOptional makes both delegations fall back to the pod's
	// in-cluster credentials, which is how this always runs. Without it the
	// options insist on an explicit kubeconfig path.
	authn := genericoptions.NewDelegatingAuthenticationOptions()
	authn.RemoteKubeConfigFileOptional = true
	if err := authn.ApplyTo(&config.Authentication, config.SecureServing, config.OpenAPIConfig); err != nil {
		return nil, fmt.Errorf("metricsapi: configure delegated authentication: %w", err)
	}

	// OpenAPI is not decoration: since server-side apply went GA, InstallAPIGroup
	// refuses a group whose v3 config is nil, because it builds the field
	// management type converter from it. Serving it also makes `kubectl explain`
	// work against this group.
	namer := openapinamer.NewDefinitionNamer(Scheme)
	config.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(metricsv1alpha1.GetOpenAPIDefinitions, namer)
	config.OpenAPIConfig.Info.Title = "simplyblock-metrics"
	config.OpenAPIConfig.Info.Version = operatorAPIVersion
	config.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(metricsv1alpha1.GetOpenAPIDefinitions, namer)
	config.OpenAPIV3Config.Info.Title = "simplyblock-metrics"
	config.OpenAPIV3Config.Info.Version = operatorAPIVersion

	authz := genericoptions.NewDelegatingAuthorizationOptions()
	authz.RemoteKubeConfigFileOptional = true
	if err := authz.ApplyTo(&config.Authorization); err != nil {
		return nil, fmt.Errorf("metricsapi: configure delegated authorization: %w", err)
	}

	completed := config.Complete()
	server, err := completed.New("metrics.simplyblock.io-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, fmt.Errorf("metricsapi: build api server: %w", err)
	}

	group := genericapiserver.NewDefaultAPIGroupInfo(metricsv1alpha1.GroupName, Scheme, ParameterCodec, Codecs)
	group.VersionedResourcesStorageMap[metricsv1alpha1.GroupVersion.Version] = map[string]rest.Storage{
		ResourceName: NewStorage(volumes, reader, capacity),
	}
	if err := server.InstallAPIGroup(&group); err != nil {
		return nil, fmt.Errorf("metricsapi: install api group: %w", err)
	}

	return &Server{server: server, log: log.WithName("metricsapi")}, nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every replica
// serves, because every replica may be the one the Service routes to.
func (s *Server) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable. It blocks until ctx is canceled, then
// drains and shuts the listener down.
func (s *Server) Start(ctx context.Context) error {
	s.log.Info("aggregated metrics API listening", "group", metricsv1alpha1.GroupName)
	if err := s.server.PrepareRun().RunWithContext(ctx); err != nil {
		return fmt.Errorf("metricsapi: serve: %w", err)
	}
	return nil
}
