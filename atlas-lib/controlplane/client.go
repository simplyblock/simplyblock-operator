package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/simplyblock/atlas/internal/cpapi"
)

// Config configures a control-plane Client.
type Config struct {
	Endpoint string        // base URL of the control-plane API
	Token    string        // cluster secret / bearer token
	Timeout  time.Duration // per-request timeout; zero means a sane default
}

// Client talks to the simplyblock control-plane v2 API. It wraps the
// generated, spec-validated client (see internal/cpapi) and exposes clean,
// domain-typed calls; the generated client is never surfaced. Resource methods
// live in per-resource files (volumes.go, pools.go, …).
type Client struct {
	cfg Config
	api cpapi.ClientWithResponsesInterface
}

// New returns a Client for the given configuration. It authenticates every
// request with a bearer token (the cluster secret).
func New(cfg Config) (*Client, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	api, err := cpapi.NewClientWithResponses(
		cfg.Endpoint,
		cpapi.WithHTTPClient(&http.Client{Timeout: cfg.Timeout}),
		cpapi.WithRequestEditorFn(bearerAuth(cfg.Token)),
	)
	if err != nil {
		return nil, fmt.Errorf("control-plane client: %w", err)
	}
	return &Client{cfg: cfg, api: api}, nil
}

func bearerAuth(token string) cpapi.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return nil
	}
}

// parseUUID parses an identifier into a UUID, wrapping failures with what the
// identifier was (for error messages).
func parseUUID(what, s string) (uuid.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%s %q: %w", what, s, err)
	}
	return u, nil
}

// parseIDs parses a cluster and pool identifier into the UUIDs the v2 API
// path parameters require.
func parseIDs(clusterID, poolID string) (cluster, pool uuid.UUID, err error) {
	if cluster, err = parseUUID("cluster id", clusterID); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if pool, err = parseUUID("pool id", poolID); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return cluster, pool, nil
}
