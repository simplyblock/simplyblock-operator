// internal/webapi/client.go

package webapi

import (
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/tlsutil"
)

type Client struct {
	BaseURL    string
	HttpClient *http.Client
	// initErr captures any setup error (e.g. failure to load the TLS CA
	// bundle when TLS is enabled). It is surfaced from request methods so
	// callers see a real error instead of silently dropping back to a
	// non-functional client.
	initErr error
}

var (
	tlsClientOnce    sync.Once
	tlsClientCacheMu sync.Mutex
	tlsClient        *http.Client
	tlsClientErr     error
)

// resetTLSClientOnce wipes the sync.Once guard. Test-only.
func resetTLSClientOnce() {
	tlsClientOnce = sync.Once{}
}

func cachedTLSClient() (*http.Client, error) {
	tlsClientOnce.Do(func() {
		ns, err := tlsutil.DetectOperatorNamespace()
		if err != nil {
			tlsClientErr = err
			return
		}
		certPath, keyPath := "", ""
		if os.Getenv("SB_TLS_CONNECT") == "authenticated" {
			certPath = tlsutil.ServiceClientCertificatePath
			keyPath = tlsutil.ServiceClientKeyPath
		}
		tlsClient, tlsClientErr = tlsutil.BuildWebAPIClient(ns, tlsutil.ServiceCABundlePath, certPath, keyPath)
	})
	return tlsClient, tlsClientErr
}

func NewClient(baseURL ...string) *Client {
	tlsEnabled := os.Getenv("SB_TLS_SERVE") == "1"

	defaultURL := "http://simplyblock-webappapi:5000"
	httpClient := &http.Client{Timeout: 30 * time.Second}
	var initErr error

	if tlsEnabled {
		defaultURL = "https://simplyblock-webappapi:5000"
		if c, err := cachedTLSClient(); err != nil {
			initErr = err
		} else {
			httpClient = c
		}
	}

	url := defaultURL
	if envURL := os.Getenv("SIMPLYBLOCK_WEBAPI_BASE_URL"); envURL != "" {
		url = envURL
	}
	if len(baseURL) > 0 {
		url = baseURL[0]
	}

	return &Client{
		BaseURL:    url,
		HttpClient: httpClient,
		initErr:    initErr,
	}
}
