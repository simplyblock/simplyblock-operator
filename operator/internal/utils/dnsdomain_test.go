// Tests for the cluster DNS domain and the in-cluster names built from it.
package utils

import (
	"slices"
	"strings"
	"testing"
)

// useClusterDNSDomain sets the domain for one test and puts the default back.
func useClusterDNSDomain(t *testing.T, domain string) {
	t.Helper()
	if err := SetClusterDNSDomain(domain); err != nil {
		t.Fatalf("SetClusterDNSDomain(%q): %v", domain, err)
	}
	t.Cleanup(func() { _ = SetClusterDNSDomain("") })
}

func TestTheClusterDNSDomainDefaultsToClusterLocal(t *testing.T) {
	if got := ClusterDNSDomain(); got != "cluster.local" {
		t.Fatalf("ClusterDNSDomain() = %q before anything set it", got)
	}
}

func TestATrailingDotIsDropped(t *testing.T) {
	useClusterDNSDomain(t, "acme.local.")
	if got := ClusterDNSDomain(); got != "acme.local" {
		t.Fatalf("ClusterDNSDomain() = %q", got)
	}
}

func TestANonDomainIsRefused(t *testing.T) {
	t.Cleanup(func() { _ = SetClusterDNSDomain("") })
	if err := SetClusterDNSDomain("Not A Domain"); err == nil {
		t.Fatal("SetClusterDNSDomain accepted a value no resolver answers for")
	}
	if got := ClusterDNSDomain(); got != "cluster.local" {
		t.Fatalf("a refused value still replaced the domain: %q", got)
	}
}

// Regression: 2026-10-12-cluster-local-hardcoded — a cluster set up with a DNS
// domain other than cluster.local got storage-node API addresses that did not
// resolve and serving certificates that did not name the service.
func TestTheStorageNodeAPIAddressFollowsTheClusterDNSDomain(t *testing.T) {
	useClusterDNSDomain(t, "acme.local")

	got := StorageNodeSetAPIAddress("worker-1", "simplyblock")
	want := "worker-1.simplyblock-storage-node-api.simplyblock.svc.acme.local:5000"
	if got != want {
		t.Fatalf("StorageNodeSetAPIAddress = %q, want %q", got, want)
	}
}

func TestTheServingCertificateNamesTheClusterDNSDomain(t *testing.T) {
	useClusterDNSDomain(t, "acme.local")

	cert := BuildServiceServingCertificate("simplyblock", "svc", "svc-tls")
	raw, _ := cert.Object["spec"].(map[string]any)["dnsNames"].([]any)
	names := make([]string, 0, len(raw))
	for _, n := range raw {
		names = append(names, n.(string))
	}

	for _, want := range []string{"svc.simplyblock.svc.acme.local", "*.svc.simplyblock.svc.acme.local"} {
		if !slices.Contains(names, want) {
			t.Errorf("dnsNames %v lack %q", names, want)
		}
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".cluster.local") {
			t.Errorf("dnsNames carry %q on a cluster whose domain is acme.local", n)
		}
	}
}
