package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestRunReusesConnectionsAcrossRequests proves the gateway actually reuses a
// connection across requests rather than forcing a fresh TCP+TLS handshake
// every time.
//
// Regression: run built embedgw.Config without KeepAlive, so it kept Go's
// zero value and versitygw's own server (DisableKeepalive: !server.keepAlive,
// opt-in) closed every connection after one response. Every request --
// including every individual UploadPart call within one multipart upload --
// then paid a fresh TCP+TLS handshake, which is consistent with issue 18's
// slow UploadPart calls and TLS handshake timeouts under concurrent load.
func TestRunReusesConnectionsAcrossRequests(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeServingCert(t, cert, key, 1)

	kube := newFakeKube(t)
	cfg := testConfig(t)
	cfg.TLSCertFile, cfg.TLSKeyFile = cert, key
	startRun(t, cfg, kube)

	waitForHealth(t, insecureClient(), "https://"+cfg.Port+healthPath)

	// A client of its own, used for nothing before the traced requests below,
	// so "reused" genuinely reflects those two calls and not the health check.
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
	httpClient := &http.Client{Transport: transport}
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String("https://" + cfg.Port),
		Credentials:  awscreds.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, ""),
		UsePathStyle: true,
		HTTPClient:   httpClient,
	})

	reused := make(chan bool, 2)
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused <- info.Reused }}
	ctx := httptrace.WithClientTrace(context.Background(), trace)

	if _, err := client.ListBuckets(ctx, &s3.ListBucketsInput{}); err != nil {
		t.Fatalf("ListBuckets #1: %v", err)
	}
	if _, err := client.ListBuckets(ctx, &s3.ListBucketsInput{}); err != nil {
		t.Fatalf("ListBuckets #2: %v", err)
	}

	first := <-reused
	second := <-reused
	t.Logf("first request reused a connection: %v; second: %v", first, second)
	if first {
		t.Fatal("the first request should not have reused anything")
	}
	if !second {
		t.Error("the second request did not reuse the first request's connection -- keep-alive is off")
	}
}
