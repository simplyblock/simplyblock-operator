// Package clusters resolves a cluster ID to a connected control-plane client.
//
// It owns the driver's cluster secret (which clusters exist, where their
// control planes are, and what credential reaches them) and is the only place
// that file is read. Three call sites used to parse it independently, each with
// its own environment-variable default and its own error handling, which is how
// a cluster could be visible to one of them and not the others.
package clusters

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/simplyblock/atlas/errs/deferrers"
	"github.com/simplyblock/atlas/lvol"
	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/controlplane"
)

const (
	// secretPathEnv names the file holding the cluster secret, and
	// defaultSecretPath is where the chart mounts it.
	secretPathEnv     = "SPDKCSI_SECRET"
	defaultSecretPath = "/etc/spdkcsi-secret/secret.json"

	// apiTokenPathEnv names a file holding an API token that supersedes the
	// per-cluster cluster_secret when it is set and readable.
	apiTokenPathEnv = "SPDKCSI_API_TOKEN_PATH"
)

// Config is one cluster's entry in the driver's secret.
type Config struct {
	ClusterID       string `json:"cluster_id"`
	ClusterEndpoint string `json:"cluster_endpoint"`
	ClusterSecret   string `json:"cluster_secret"`
}

// Info is the secret file as a whole.
type Info struct {
	Clusters []Config `json:"clusters"`
}

// ParseJSONFile decodes the JSON document in fileName into result.
func ParseJSONFile(fileName string, result interface{}) error {
	file, err := os.Open(fileName)
	if err != nil {
		return err
	}
	defer deferrers.Close(file)

	bytes, err := io.ReadAll(file)
	if err != nil {
		return err
	}

	return json.Unmarshal(bytes, result)
}

// FromEnv returns the value of environment variable env, or def when it is
// unset or empty.
func FromEnv(env, def string) string {
	s := os.Getenv(env)
	if s != "" {
		return s
	}
	return def
}

// SecretPath is the file the cluster secret is read from.
func SecretPath() string {
	return FromEnv(secretPathEnv, defaultSecretPath)
}

// Load reads the driver's cluster secret.
func Load() (Info, error) {
	var clusters Info
	if err := ParseJSONFile(SecretPath(), &clusters); err != nil {
		return Info{}, fmt.Errorf("failed to parse secret file: %w", err)
	}
	return clusters, nil
}

// List returns the ID of every cluster in the secret.
func List() ([]string, error) {
	clusters, err := Load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(clusters.Clusters))
	for _, cluster := range clusters.Clusters {
		ids = append(ids, cluster.ClusterID)
	}
	return ids, nil
}

// Client creates a control-plane client scoped to a cluster and optionally a
// pool. poolIDOrName may be a pool UUID (used as-is), a pool name (resolved via
// the API), or empty (no pool context, so only cluster-level operations work).
func Client(ctx context.Context, clusterID, poolIDOrName string) (*controlplane.ClusterClient, error) {
	clusters, err := Load()
	if err != nil {
		return nil, err
	}

	var clusterConfig *Config
	for _, cluster := range clusters.Clusters {
		if cluster.ClusterID == clusterID {
			clusterConfig = &cluster
			break
		}
	}

	if clusterConfig == nil {
		return nil, fmt.Errorf("failed to find secret for clusterID %s: %w", clusterID, controlplane.ErrClusterNotFound)
	}

	if clusterConfig.ClusterEndpoint == "" {
		return nil, fmt.Errorf("invalid cluster configuration for clusterID %s: missing endpoint", clusterID)
	}

	credential := credentialFor(clusterConfig)
	if credential == "" {
		return nil, fmt.Errorf(
			"invalid cluster configuration for clusterID %s: no cluster_secret and no API token available",
			clusterID,
		)
	}

	klog.Infof("Simplyblock client created for ClusterID:%s, Endpoint:%s",
		clusterConfig.ClusterID,
		clusterConfig.ClusterEndpoint,
	)

	client, err := controlplane.NewClusterClient(clusterID, clusterConfig.ClusterEndpoint, credential)
	if err != nil {
		return nil, err
	}

	if poolIDOrName != "" {
		poolUUID, err := resolvePoolUUID(ctx, client, poolIDOrName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve pool %q: %w", poolIDOrName, err)
		}
		client.ScopeToPool(poolUUID)
	}

	return client, nil
}

// credentialFor returns the bearer credential for a cluster: the API token when
// SPDKCSI_API_TOKEN_PATH is set and names a readable, non-empty file, otherwise
// the cluster_secret from the secret entry.
func credentialFor(cfg *Config) string {
	tokenPath := os.Getenv(apiTokenPathEnv)
	if tokenPath == "" {
		return cfg.ClusterSecret
	}

	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		klog.Warningf(
			"%s is set but token file %q could not be read for cluster %s: %v; falling back to cluster_secret",
			apiTokenPathEnv, tokenPath, cfg.ClusterID, err,
		)
		return cfg.ClusterSecret
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		klog.Warningf("%s is set but token file %q is empty for cluster %s; falling back to cluster_secret",
			apiTokenPathEnv, tokenPath, cfg.ClusterID)
		return cfg.ClusterSecret
	}
	klog.Infof("Using API token from file for cluster %s", cfg.ClusterID)
	return token
}

// resolvePoolUUID returns poolIDOrName as-is if it is already a UUID, otherwise
// looks up the pool UUID by name via the API.
func resolvePoolUUID(ctx context.Context, c *controlplane.ClusterClient, poolIDOrName string) (string, error) {
	if lvol.IsCanonicalUUID(poolIDOrName) {
		return poolIDOrName, nil
	}
	return c.GetPoolUUIDByName(ctx, poolIDOrName)
}
