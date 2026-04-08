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

type DetailedResponse struct {
	Body    []byte
	Status  int
	Headers http.Header
}

func (c *Client) Do(
	ctx context.Context,
	clusterSecret string,
	method string,
	endpoint string,
	body interface{},
) ([]byte, int, error) {
	resp, err := c.DoDetailed(ctx, clusterSecret, method, endpoint, body)
	if err != nil {
		return nil, resp.Status, err
	}

	return resp.Body, resp.Status, nil
}

func (c *Client) DoDetailed(
	ctx context.Context,
	clusterSecret string,
	method string,
	endpoint string,
	body interface{},
) (DetailedResponse, error) {
	// Build URL
	url := fmt.Sprintf("%s%s", c.BaseURL, endpoint)

	// Encode JSON body if provided
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return DetailedResponse{}, fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewBuffer(b)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return DetailedResponse{}, fmt.Errorf("create request: %w", err)
	}

	// Attach auth header
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", clusterSecret))
	req.Header.Set("Content-Type", "application/json")

	// Execute the request
	resp, err := c.HttpClient.Do(req)
	if err != nil {
		return DetailedResponse{}, fmt.Errorf("http error: %w", err)
	}

	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			fmt.Printf("warning: failed to close response body: %v\n", cerr)
		}
	}()

	// Read raw response body
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return DetailedResponse{Status: resp.StatusCode, Headers: resp.Header.Clone()}, fmt.Errorf("read body: %w", err)
	}

	return DetailedResponse{
		Body:    data,
		Status:  resp.StatusCode,
		Headers: resp.Header.Clone(),
	}, nil
}
