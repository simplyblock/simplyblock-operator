// Config reads the gateway's startup configuration from the environment, the
// mechanism design-objectstore.md's ObjectStore.spec.envFrom passes through
// from the operator's gateway Deployment. Kept separate from cmd/gateway so
// the parsing and defaulting rules are covered by table tests without
// running the gateway itself.

package config

import (
	"fmt"
	"os"
	"strconv"
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
}

// Environment variable names. The three with two names accept either:
// versitygw's own CLI defines both per field (cmd/versitygw/main.go), and
// FromEnv reads whichever is set.
const (
	envRootDir            = "GATEWAY_ROOT_DIR"
	envPort               = "VGW_PORT"
	envRootAccessKeyID    = "ROOT_ACCESS_KEY_ID"
	envRootAccessKeyIDAlt = "ROOT_ACCESS_KEY"
	envRootSecretKey      = "ROOT_SECRET_ACCESS_KEY"
	envRootSecretKeyAlt   = "ROOT_SECRET_KEY"
	envBucketLinks        = "GATEWAY_BUCKET_LINKS"
	envFsync              = "GATEWAY_FSYNC"
	defaultPort           = ":9000"
	defaultBucketLinks    = true
	defaultFsync          = true
)

// FromEnv reads a Config from the process environment. RootDir,
// RootAccessKeyID, and RootSecretAccessKey have no safe default and are
// required; Port, BucketLinks, and Fsync default as documented on Config,
// and an invalid (non-boolean) value for either flag is reported rather than
// silently falling back.
func FromEnv() (Config, error) {
	cfg := Config{
		RootDir:             os.Getenv(envRootDir),
		Port:                getenvDefault(envPort, defaultPort),
		RootAccessKeyID:     firstNonEmpty(os.Getenv(envRootAccessKeyID), os.Getenv(envRootAccessKeyIDAlt)),
		RootSecretAccessKey: firstNonEmpty(os.Getenv(envRootSecretKey), os.Getenv(envRootSecretKeyAlt)),
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
	return cfg, nil
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
