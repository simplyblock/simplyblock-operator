package webapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestNewClientDefaultsToHTTP(t *testing.T) {
	t.Setenv("TLS_ENABLED", "")
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")

	c := NewClient()
	if c.BaseURL != "http://simplyblock-webappapi:5000" {
		t.Fatalf("BaseURL = %q, want http://simplyblock-webappapi:5000", c.BaseURL)
	}
	if c.HttpClient == nil {
		t.Fatalf("HttpClient unset")
	}
	if c.initErr != nil {
		t.Fatalf("unexpected initErr: %v", c.initErr)
	}
}

func TestNewClientTLSEnabledFlipsScheme(t *testing.T) {
	t.Setenv("TLS_ENABLED", "true")
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)

	c := NewClient()
	if !strings.HasPrefix(c.BaseURL, "https://") {
		t.Fatalf("BaseURL = %q, want https:// scheme when TLS enabled", c.BaseURL)
	}
	// CA bundle won't exist in unit tests — initErr should be set so callers
	// surface a real error rather than silently using a misconfigured client.
	if c.initErr == nil {
		t.Fatalf("expected initErr when CA bundle is unavailable in unit tests")
	}
}

func TestNewClientInitErrSurfacesFromDo(t *testing.T) {
	t.Setenv("TLS_ENABLED", "true")
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)

	c := NewClient()
	if c.initErr == nil {
		t.Skip("CA bundle unexpectedly present in test environment")
	}

	_, _, err := c.Do(context.Background(), "secret", http.MethodGet, "/api/v2/anything", nil)
	if err == nil {
		t.Fatalf("expected init error from Do")
	}
	if !strings.Contains(err.Error(), "webapi client init") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewClientExplicitURLBypassesEnv(t *testing.T) {
	t.Setenv("TLS_ENABLED", "true")
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "https://override.example/")
	resetTLSClientCacheForTest(t)

	c := NewClient("http://explicit.example:1234")
	if c.BaseURL != "http://explicit.example:1234" {
		t.Fatalf("explicit baseURL not honored: %q", c.BaseURL)
	}
}

// resetTLSClientCacheForTest forces the next NewClient call to rebuild its
// cached TLS client so test order doesn't leak state.
func resetTLSClientCacheForTest(t *testing.T) {
	t.Helper()
	tlsClientCacheMu.Lock()
	defer tlsClientCacheMu.Unlock()
	tlsClient = nil
	tlsClientErr = nil
	resetTLSClientOnce()
}
