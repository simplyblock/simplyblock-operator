// Config reads the gateway's startup configuration from the environment, the
// mechanism design-objectstore.md's ObjectStore.spec.envFrom passes through
// from the operator's gateway Deployment. Kept separate from cmd/gateway so
// the parsing and defaulting rules are covered by table tests without
// running the gateway itself.

package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"
)

// Config is everything cmd/gateway needs to start the embedded VersityGW
// server. RootAccessKeyID, RootSecretAccessKey, and VGW_PORT reuse
// versitygw's own env var names (cmd/versitygw/main.go) so an operator who
// already knows versitygw recognizes them; the GATEWAY_-prefixed names are
// this project's own, for settings versitygw's CLI has no flag for at this
// embedding layer.
type Config struct {
	// RootDir is the directory backing every bucket this gateway serves,
	// bind-mounted in from the pNFS (or local path) mount DeliverBucket
	// (csi-driver) delivers. Required: there is no directory a gateway
	// could guess belongs to it.
	RootDir string

	// Port is the address embedgw's S3 API listens on: "host:port" or
	// ":port." Defaults to ":9000," design-objectstore.md's own sample
	// port, when unset.
	Port string

	// RootAccessKeyID and RootSecretAccessKey are the gateway's one
	// administrative account (embedgw.Config.RootUserAccess/
	// RootUserSecret). Both required: nothing here can start the S3 API
	// without them, and defaulting one while requiring the other would
	// start a server against half a credential an operator picked.
	RootAccessKeyID     string
	RootSecretAccessKey string

	// BucketLinks enables bucket-per-volume via directory symlinks
	// (docs/adr/0002-bucket-per-volume-via-bucketlinks.md), the shape every
	// bucket this gateway serves takes: an S3Bucket's own volume, never a
	// directory this gateway created itself. Defaults to true, since a
	// gateway that silently ran with this off would list no buckets at all
	// rather than fail loudly.
	BucketLinks bool

	// Fsync flushes an object's data, metadata, and directory entry to
	// stable storage before PutObject acknowledges it. Defaults to true:
	// durability is the reason this project chose versitygw's posix
	// backend's --fsync flag over its off-by-default posture
	// (notes/versitygw-put-durability.md), so a bad env value here falls
	// back to the safe default rather than the library's unsafe one.
	Fsync bool

	// ObjectStore and Namespace identify the ObjectStore this gateway
	// serves, and the namespace its S3Buckets are created in. The operator
	// sets both on the gateway Deployment.
	ObjectStore string
	Namespace   string

	// BucketStorageClass and BucketCapacityBytes are what every S3Bucket
	// CreateBucket creates is provisioned with: the ObjectStore's
	// spec.buckets, set by the operator.
	BucketStorageClass  string
	BucketCapacityBytes int64

	// BucketTimeout bounds how long CreateBucket waits for an S3Bucket to
	// become Ready, and DeleteBucket for one to be gone. A CreateBucket that
	// runs out of it deletes the S3Bucket again.
	BucketTimeout time.Duration

	// Vault configures versitygw's built-in HashiCorp Vault IAM backend
	// (docs/adr/0007-gateway-iam-is-versitygw-vault-backend.md). An empty
	// Vault.EndpointURL leaves the gateway in single-account mode, holding
	// only the root account.
	Vault Vault

	// IAMCacheTTL and IAMCachePrune are, in seconds, how long a gateway
	// replica caches an IAM account and how often it drops expired entries.
	// embedgw passes zero values straight to versitygw's cache, so both
	// default to versitygw's own CLI defaults (120 and 3600) rather than to
	// zero.
	IAMCacheTTL   int
	IAMCachePrune int
}

// Vault is versitygw's Vault IAM backend configuration, one field per
// embedgw.Config Vault field and read from the same VGW_IAM_VAULT_* names
// versitygw's own CLI uses (cmd/versitygw/main.go). ServerCert, ClientCert,
// and ClientCertKey are PEM contents, not paths: auth/iam_vault.go loads them
// from bytes, whatever embedgw.Config's doc comments say.
type Vault struct {
	EndpointURL            string
	Namespace              string
	SecretStoragePath      string
	SecretStorageNamespace string
	AuthMethod             string
	AuthNamespace          string
	MountPath              string
	RootToken              string
	RoleID                 string
	RoleSecret             string
	ServerCert             string
	ClientCert             string
	ClientCertKey          string
}

// Environment variable names. The three with two names accept either:
// versitygw's own CLI defines both per field (cmd/versitygw/main.go), and
// FromEnv reads whichever is set.
const (
	envRootDir             = "GATEWAY_ROOT_DIR"
	envPort                = "VGW_PORT"
	envRootAccessKeyID     = "ROOT_ACCESS_KEY_ID"
	envRootAccessKeyIDAlt  = "ROOT_ACCESS_KEY"
	envRootSecretKey       = "ROOT_SECRET_ACCESS_KEY"
	envRootSecretKeyAlt    = "ROOT_SECRET_KEY"
	envBucketLinks         = "GATEWAY_BUCKET_LINKS"
	envFsync               = "GATEWAY_FSYNC"
	envObjectStore         = "GATEWAY_OBJECTSTORE"
	envNamespace           = "GATEWAY_NAMESPACE"
	envBucketStorageClass  = "GATEWAY_BUCKET_STORAGE_CLASS"
	envBucketCapacityBytes = "GATEWAY_BUCKET_CAPACITY_BYTES"
	envBucketTimeout       = "GATEWAY_BUCKET_TIMEOUT"
	envIAMCacheTTL         = "VGW_IAM_CACHE_TTL"
	envIAMCachePrune       = "VGW_IAM_CACHE_PRUNE"
	envVaultEndpointURL    = "VGW_IAM_VAULT_ENDPOINT_URL"
	defaultPort            = ":9000"
	defaultIAMCacheTTL     = 120
	defaultIAMCachePrune   = 3600
	defaultBucketLinks     = true
	defaultFsync           = true
	defaultBucketTimeout   = 5 * time.Minute
)

// vaultEnv names the environment variable for each Vault field, in
// versitygw's own spelling.
func vaultEnv(v *Vault) map[string]*string {
	return map[string]*string{
		envVaultEndpointURL:                      &v.EndpointURL,
		"VGW_IAM_VAULT_NAMESPACE":                &v.Namespace,
		"VGW_IAM_VAULT_SECRET_STORAGE_PATH":      &v.SecretStoragePath,
		"VGW_IAM_VAULT_SECRET_STORAGE_NAMESPACE": &v.SecretStorageNamespace,
		"VGW_IAM_VAULT_AUTH_METHOD":              &v.AuthMethod,
		"VGW_IAM_VAULT_AUTH_NAMESPACE":           &v.AuthNamespace,
		"VGW_IAM_VAULT_MOUNT_PATH":               &v.MountPath,
		"VGW_IAM_VAULT_ROOT_TOKEN":               &v.RootToken,
		"VGW_IAM_VAULT_ROLE_ID":                  &v.RoleID,
		"VGW_IAM_VAULT_ROLE_SECRET":              &v.RoleSecret,
		"VGW_IAM_VAULT_SERVER_CERT":              &v.ServerCert,
		"VGW_IAM_VAULT_CLIENT_CERT":              &v.ClientCert,
		"VGW_IAM_VAULT_CLIENT_CERT_KEY":          &v.ClientCertKey,
	}
}

// FromEnv reads a Config from the process environment. RootDir, the root
// credentials, and the bucket settings the operator sets have no safe
// default and are required; Port, BucketLinks, Fsync, and BucketTimeout
// default as documented on Config, and an invalid value for any of them is
// reported rather than silently falling back.
func FromEnv() (Config, error) {
	cfg := Config{
		RootDir:             os.Getenv(envRootDir),
		Port:                getenvDefault(envPort, defaultPort),
		RootAccessKeyID:     firstNonEmpty(os.Getenv(envRootAccessKeyID), os.Getenv(envRootAccessKeyIDAlt)),
		RootSecretAccessKey: firstNonEmpty(os.Getenv(envRootSecretKey), os.Getenv(envRootSecretKeyAlt)),
		ObjectStore:         os.Getenv(envObjectStore),
		Namespace:           os.Getenv(envNamespace),
		BucketStorageClass:  os.Getenv(envBucketStorageClass),
	}

	var missing []string
	if cfg.RootDir == "" {
		missing = append(missing, envRootDir)
	}
	if cfg.RootAccessKeyID == "" {
		missing = append(missing, envRootAccessKeyID)
	}
	if cfg.RootSecretAccessKey == "" {
		missing = append(missing, envRootSecretKey)
	}
	for _, required := range []struct{ key, value string }{
		{envObjectStore, cfg.ObjectStore},
		{envNamespace, cfg.Namespace},
		{envBucketStorageClass, cfg.BucketStorageClass},
		{envBucketCapacityBytes, os.Getenv(envBucketCapacityBytes)},
	} {
		if required.value == "" {
			missing = append(missing, required.key)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variable(s): %v", missing)
	}

	var err error
	if cfg.BucketLinks, err = getenvBool(envBucketLinks, defaultBucketLinks); err != nil {
		return Config{}, err
	}
	if cfg.Fsync, err = getenvBool(envFsync, defaultFsync); err != nil {
		return Config{}, err
	}
	capacity := os.Getenv(envBucketCapacityBytes)
	if cfg.BucketCapacityBytes, err = strconv.ParseInt(capacity, 10, 64); err != nil || cfg.BucketCapacityBytes < 1 {
		return Config{}, fmt.Errorf("%s=%q is not a positive byte count", envBucketCapacityBytes, capacity)
	}
	if cfg.BucketTimeout, err = getenvDuration(envBucketTimeout, defaultBucketTimeout); err != nil {
		return Config{}, err
	}
	if cfg.IAMCacheTTL, err = getenvSeconds(envIAMCacheTTL, defaultIAMCacheTTL); err != nil {
		return Config{}, err
	}
	if cfg.IAMCachePrune, err = getenvSeconds(envIAMCachePrune, defaultIAMCachePrune); err != nil {
		return Config{}, err
	}
	if cfg.Vault, err = vaultFromEnv(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// vaultFromEnv reads the Vault IAM backend's settings. Any of them set
// without VGW_IAM_VAULT_ENDPOINT_URL is an error: versitygw would ignore them
// and run single-account, so every account but root would fail to
// authenticate.
func vaultFromEnv() (Vault, error) {
	var v Vault
	var orphaned []string
	for key, field := range vaultEnv(&v) {
		*field = os.Getenv(key)
		if *field != "" && key != envVaultEndpointURL {
			orphaned = append(orphaned, key)
		}
	}
	if v.EndpointURL == "" && len(orphaned) > 0 {
		sort.Strings(orphaned)
		return Vault{}, fmt.Errorf("%v set without %s", orphaned, envVaultEndpointURL)
	}
	return v, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvBool(key string, fallback bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a valid boolean: %w", key, v, err)
	}
	return parsed, nil
}

func getenvDuration(key string, fallback time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(v)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s=%q is not a positive duration", key, v)
	}
	return parsed, nil
}

func getenvSeconds(key string, fallback int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(v)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s=%q is not a positive number of seconds", key, v)
	}
	return parsed, nil
}
