package controlplane

import (
	"context"
	"net/http"
	"time"

	"github.com/simplyblock/atlas/lvol"
)

// Config configures a control-plane Client.
type Config struct {
	Endpoint string        // base URL of the control-plane API
	Token    string        // bearer/cluster token
	Timeout  time.Duration // per-request timeout; zero means a sane default
}

// Client talks to the simplyblock control-plane API.
type Client struct {
	cfg  Config
	http *http.Client
}

// Client implements lvol.Resolver.
var _ lvol.Resolver = (*Client)(nil)

// New returns a Client for the given configuration.
func New(cfg Config) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout},
	}
}

// Volume fetches the identity of a logical volume by handle.
func (c *Client) Volume(ctx context.Context, h lvol.VolumeHandle) (lvol.Volume, error) {
	// TODO: issue the request against c.cfg.Endpoint using c.http.
	return lvol.Volume{}, nil
}

// Connection fetches how to reach a logical volume over NVMe-oF.
func (c *Client) Connection(ctx context.Context, h lvol.VolumeHandle) (lvol.Connection, error) {
	// TODO: issue the request against c.cfg.Endpoint using c.http.
	return lvol.Connection{}, nil
}
