// internal/webapi/client.go

package webapi

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/tlsutil"
)

var ServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

type Client struct {
	BaseURL    string
	HttpClient *http.Client
	// initErr captures any setup error (for example, failure to load the TLS CA
	// bundle when TLS is enabled). It is surfaced from request methods so
	// callers see a real error instead of silently dropping back to a
	// non-functional client.
	initErr error
	saToken string
}

var (
	tlsClientCacheMu sync.Mutex
	tlsClient        *http.Client
)

// cachedTLSClient is the verified client every TLS request shares.
//
// Only a success is remembered. The CA bundle and the client certificate arrive
// with the pod's volumes, and a failure cached for the life of the process would
// leave an operator that asked early unable to reach a control plane that has
// been answering for hours.
func cachedTLSClient() (*http.Client, error) {
	tlsClientCacheMu.Lock()
	defer tlsClientCacheMu.Unlock()
	if tlsClient != nil {
		return tlsClient, nil
	}

	ns, err := tlsutil.DetectOperatorNamespace()
	if err != nil {
		return nil, err
	}
	// The pair is presented where the pod mounts one. Whether callers present a
	// certificate is the ControlPlane's decision, and the chart mounts the pair
	// exactly when the ControlPlane asks for it, so the files are the evidence.
	certPath, keyPath := tlsutil.ServiceClientCertificatePath, tlsutil.ServiceClientKeyPath
	if !fileExists(certPath) || !fileExists(keyPath) {
		certPath, keyPath = "", ""
	}
	client, err := tlsutil.BuildWebAPIClient(ns, tlsutil.ServiceCABundlePath, certPath, keyPath)
	if err != nil {
		return nil, err
	}
	// Both transports wait as long, so a step's claim lease measured
	// against RequestTimeout covers a call made over either.
	client.Timeout = RequestTimeout
	tlsClient = client
	return tlsClient, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// RequestTimeout bounds one request to the control plane. A claim's lease on a
// step is measured against it: a lease no longer than this can expire while
// the call it covers is still waiting for an answer.
const RequestTimeout = 30 * time.Second

func NewClient(baseURL ...string) *Client {
	const defaultURL = "http://" + defaultHost

	url, explicit := defaultURL, false
	if envURL := os.Getenv("SIMPLYBLOCK_WEBAPI_BASE_URL"); envURL != "" {
		url, explicit = envURL, true
	}
	if len(baseURL) > 0 {
		url, explicit = baseURL[0], true
	}

	httpClient := &http.Client{Timeout: RequestTimeout}
	var initErr error
	switch {
	case !explicit:
		// The default address, whose scheme the ControlPlane decides per request.
		httpClient.Transport = &adaptiveTransport{plain: http.DefaultTransport, secure: secureTransport}
	case strings.HasPrefix(url, "https://"):
		if c, err := cachedTLSClient(); err != nil {
			initErr = err
		} else {
			httpClient = c
		}
	}

	c := &Client{
		BaseURL:    url,
		HttpClient: httpClient,
		initErr:    initErr,
	}

	// Read SA token (best-effort; empty string if unavailable)
	if data, err := os.ReadFile(ServiceAccountTokenPath); err == nil {
		c.saToken = strings.TrimSpace(string(data))
	}

	return c
}

// StreamClient carries what a long-lived streaming request needs against the
// control-plane API: the resolved base URL and an HTTP client that shares the
// operator's TLS/mTLS configuration but has no request timeout (a fixed timeout
// would sever a stream).
type StreamClient struct {
	BaseURL string
	Client  *http.Client
}

// NewStreamClient resolves the endpoint and TLS setup exactly as [NewClient],
// but returns a client suitable for streaming: it shares NewClient's transport
// (and thus its TLS/mTLS config) without the request timeout. Unlike NewClient
// it surfaces the TLS-setup error instead of deferring it to the first request.
func NewStreamClient(baseURL ...string) (*StreamClient, error) {
	base := NewClient(baseURL...)
	if base.initErr != nil {
		return nil, base.initErr
	}
	transport := http.DefaultTransport
	if base.HttpClient != nil && base.HttpClient.Transport != nil {
		transport = base.HttpClient.Transport
	}
	return &StreamClient{
		BaseURL: base.BaseURL,
		Client:  &http.Client{Transport: transport},
	}, nil
}
