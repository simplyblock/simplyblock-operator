// Proves run (main.go) serves the configured certificate over TLS and picks up
// a rotated one without a restart, the way cert-manager and OpenShift's
// service CA rewrite the Secret the operator mounts. Certificates are
// generated per test, so nothing here depends on files in the repository.

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// writeServingCert writes a self-signed certificate for 127.0.0.1 with the
// given serial number, and its key, to cert and key.
func writeServingCert(t *testing.T, cert, key string, serial int64) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "gateway-test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("creating the certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("encoding the key: %v", err)
	}
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("writing the certificate: %v", err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("writing the key: %v", err)
	}
}

// startTLSGateway runs the gateway serving cert and key, with the certificate
// re-read every few milliseconds, waits for it to answer over HTTPS, and
// returns its address.
func startTLSGateway(t *testing.T, cert, key string) string {
	t.Helper()
	previous := certPollInterval
	certPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { certPollInterval = previous })

	cfg := testConfig(t)
	cfg.TLSCertFile, cfg.TLSKeyFile = cert, key
	startRun(t, cfg, newFakeKube(t))
	waitForHealth(t, insecureClient(), "https://"+cfg.Port+healthPath)
	return cfg.Port
}

// insecureClient skips verification so a test can read whichever certificate
// the gateway presents, and checks the serial number itself.
func insecureClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the test inspects the certificate itself
	}}
}

// servedSerial is the serial number of the certificate the gateway at addr
// presents in a TLS handshake.
func servedSerial(t *testing.T, addr string) int64 {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the test inspects the certificate itself
	if err != nil {
		t.Fatalf("TLS handshake with %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

func TestRunServesS3OverTLSWithTheConfiguredCertificate(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeServingCert(t, cert, key, 1)

	addr := startTLSGateway(t, cert, key)

	if got := servedSerial(t, addr); got != 1 {
		t.Errorf("served certificate serial = %d, want 1", got)
	}
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String("https://" + addr),
		Credentials:  awscreds.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, ""),
		UsePathStyle: true,
		HTTPClient:   insecureClient(),
	})
	if _, err := client.ListBuckets(context.Background(), &s3.ListBucketsInput{}); err != nil {
		t.Errorf("ListBuckets over TLS: %v", err)
	}
	resp, err := http.Get("http://" + addr + healthPath) //nolint:noctx // one request against a local server
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("plain HTTP health check succeeded, want the TLS listener to refuse it")
		}
	}
}

func TestRunServesARotatedCertificateWithoutARestart(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeServingCert(t, cert, key, 1)
	addr := startTLSGateway(t, cert, key)

	writeServingCert(t, cert, key, 2)

	deadline := time.Now().Add(5 * time.Second)
	for servedSerial(t, addr) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("the gateway still serves serial 1 five seconds after the files were rewritten with serial 2")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
