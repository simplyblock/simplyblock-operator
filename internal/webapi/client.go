// internal/webapi/client.go

package webapi

import (
	"net/http"
	"os"
	"time"
)

type Client struct {
	BaseURL    string
	HttpClient *http.Client
}

func NewClient(baseURL ...string) *Client {
	url := "http://simplyblock-webappapi:5000"
	if envURL := os.Getenv("SIMPLYBLOCK_WEBAPI_BASE_URL"); envURL != "" {
		url = envURL
	}
	if len(baseURL) > 0 {
		url = baseURL[0]
	}

	return &Client{
		BaseURL: url,
		HttpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}
