// internal/webapi/request.go

package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func (c *Client) Do(
	ctx context.Context,
	clusterSecret string,
	method string,
	endpoint string,
	body interface{},
) ([]byte, int, error) {
	data, _, status, err := c.DoWithHeaders(ctx, clusterSecret, method, endpoint, body)
	return data, status, err
}

func (c *Client) DoWithHeaders(
	ctx context.Context,
	clusterSecret string,
	method string,
	endpoint string,
	body interface{},
) ([]byte, http.Header, int, error) {

	if c.initErr != nil {
		return nil, nil, 0, fmt.Errorf("webapi client init: %w", c.initErr)
	}

	// Build URL
	url := fmt.Sprintf("%s%s", c.BaseURL, endpoint)

	// Encode JSON body if provided
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewBuffer(b)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("create request: %w", err)
	}

	// Attach auth header
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", clusterSecret))
	req.Header.Set("Content-Type", "application/json")

	// Execute the request
	resp, err := c.HttpClient.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("http error: %w", err)
	}

	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			fmt.Printf("warning: failed to close response body: %v\n", cerr)
		}
	}()

	// Read raw response body
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}

	return data, resp.Header.Clone(), resp.StatusCode, nil
}
