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
	url := "http://webapi"
	if len(baseURL) > 0 {
		url = baseURL[0]
	}

	return &Client{
		BaseURL: url,
		HttpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}
