// internal/webapi/client.go

package webapi

import (
	"net/http"
	"time"
)

type Client struct {
	BaseURL    string
	HttpClient *http.Client
}

func NewClient(baseURL ...string) *Client {
	url := "http://simplyblock-webappapi:5000"
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
