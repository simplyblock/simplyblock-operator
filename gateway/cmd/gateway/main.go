// The gateway binary's entry point: loads Config from the environment, builds
// versitygw's own posix backend from it, wraps it so CreateBucket and
// DeleteBucket drive S3Buckets (internal/buckets), and runs the embedded
// VersityGW server until SIGINT/SIGTERM.
//
// No backend or auth.IAMService of this project's own beyond that wrapper:
// backend/posix.New already does bucket-per-volume POSIX storage with
// --bucketlinks and --fsync (docs/adr/0002-bucket-per-volume-via-
// bucketlinks.md, notes/versitygw-put-durability.md), and IAM is
// versitygw's own Vault backend, selected by embedgw.Config's Vault fields
// (docs/adr/0007-gateway-iam-is-versitygw-vault-backend.md). Without
// VGW_IAM_VAULT_ENDPOINT_URL, embedgw.RunVersityGW falls back to a
// single-account IAM service holding only the root account. Writing either a
// backend or an IAM service again here would be the second copy the
// new-files skill's search-before-writing rule exists to catch.
package main

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/versity/versitygw/backend/meta"
	"github.com/versity/versitygw/backend/posix"
	"github.com/versity/versitygw/embedgw"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kubeconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/simplyblock/gateway/internal/buckets"
	"github.com/simplyblock/gateway/internal/certwatch"
	"github.com/simplyblock/gateway/internal/config"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// versitygw has no tagged release that matches cmd/versitygw/main.go's own
// numbers forever, so these three are read from its source directly rather
// than left at embedgw.Config's zero value: a zero MaxConnections/
// MaxRequests/MultipartMaxParts rejects every connection, request, and
// multipart upload at runtime (docs/adr/0003-embedgw-spike-result.md).
const (
	maxConnections    = 250000
	maxRequests       = 100000
	multipartMaxParts = 10000

	healthPath = "/healthz"

	// bucketPollInterval is how often CreateBucket and DeleteBucket re-read
	// the S3Bucket they wait on.
	bucketPollInterval = time.Second
)

// certPollInterval is how often the serving certificate and key are re-read
// for a rotation. A var only so a test can shorten it.
var certPollInterval = 30 * time.Second

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("loading configuration: %v", err)
	}

	cli, err := kubeClient()
	if err != nil {
		log.Fatalf("building the Kubernetes client: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, cli); err != nil {
		log.Fatal(err)
	}
}

// kubeClient is a client for S3Buckets, authenticated as the pod's
// ServiceAccount in cluster.
func kubeClient() (client.Client, error) {
	restConfig, err := kubeconfig.GetConfig()
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return client.New(restConfig, client.Options{Scheme: scheme})
}

// run builds the posix backend and the embedded gateway from cfg and runs
// until ctx is done, which is main's own job split out so a test can drive it
// against a temporary directory, a fake client, and a context it controls,
// without main's os.Exit-on-error or its tie to process signals. A configured
// serving certificate is re-read whenever its files change
// (internal/certwatch), so a rotated certificate is served without a restart.
func run(ctx context.Context, cfg config.Config, cli client.Client) error {
	be, err := posix.New(cfg.RootDir, meta.XattrMeta{}, posix.PosixOpts{
		BucketLinks: cfg.BucketLinks,
		Fsync:       cfg.Fsync,
		// Every bucket this gateway serves is an S3Bucket's own volume,
		// symlinked in rather than created through CreateBucket
		// (design-bucket-provisioning.md §5), so nothing validates a
		// bucket's name against S3's naming rules at creation time the way
		// a CreateBucket call would. This is what posix.PosixOpts's own doc
		// comment calls out that case for.
		ValidateBucketNames: true,
	})
	if err != nil {
		return fmt.Errorf("starting the posix backend at %s: %w", cfg.RootDir, err)
	}

	gateway := buckets.New(be, cli, buckets.Settings{
		RootDir:          cfg.RootDir,
		Namespace:        cfg.Namespace,
		ObjectStore:      cfg.ObjectStore,
		StorageClassName: cfg.BucketStorageClass,
		CapacityBytes:    cfg.BucketCapacityBytes,
		Timeout:          cfg.BucketTimeout,
		PollInterval:     bucketPollInterval,
	})

	gwConfig := gatewayConfig(cfg)
	if cfg.TLSCertFile != "" {
		gwConfig.SigHup = certwatch.Changes(ctx, certPollInterval, cfg.TLSCertFile, cfg.TLSKeyFile)
	}
	if err := embedgw.RunVersityGW(ctx, gateway, gwConfig); err != nil {
		return fmt.Errorf("running the gateway: %w", err)
	}
	return nil
}

// gatewayConfig is the embedgw.Config cfg describes, split out of run so a
// test can check what reaches embedgw without starting a server.
func gatewayConfig(cfg config.Config) *embedgw.Config {
	return &embedgw.Config{
		RootUserAccess:    cfg.RootAccessKeyID,
		RootUserSecret:    cfg.RootSecretAccessKey,
		Ports:             []string{cfg.Port},
		HealthPath:        healthPath,
		MaxConnections:    maxConnections,
		MaxRequests:       maxRequests,
		MultipartMaxParts: multipartMaxParts,

		VaultEndpointURL:            cfg.Vault.EndpointURL,
		VaultNamespace:              cfg.Vault.Namespace,
		VaultSecretStoragePath:      cfg.Vault.SecretStoragePath,
		VaultSecretStorageNamespace: cfg.Vault.SecretStorageNamespace,
		VaultAuthMethod:             cfg.Vault.AuthMethod,
		VaultAuthNamespace:          cfg.Vault.AuthNamespace,
		VaultMountPath:              cfg.Vault.MountPath,
		VaultRootToken:              cfg.Vault.RootToken,
		VaultRoleID:                 cfg.Vault.RoleID,
		VaultRoleSecret:             cfg.Vault.RoleSecret,
		VaultServerCert:             cfg.Vault.ServerCert,
		VaultClientCert:             cfg.Vault.ClientCert,
		VaultClientCertKey:          cfg.Vault.ClientCertKey,
		IAMCacheTTL:                 cfg.IAMCacheTTL,
		IAMCachePrune:               cfg.IAMCachePrune,

		CertFile: cfg.TLSCertFile,
		KeyFile:  cfg.TLSKeyFile,
	}
}
