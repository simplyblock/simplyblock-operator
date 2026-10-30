// Tests for the cluster DNS domain, its detection, and the in-cluster names
// built from it.
package utils

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	atlaskube "github.com/simplyblock/atlas/kube"
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

// writeResolvConf writes a resolver file and returns its path.
func writeResolvConf(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write the resolver file: %v", err)
	}
	return path
}

func TestTheDomainIsReadFromTheResolverSearchList(t *testing.T) {
	path := writeResolvConf(t, "nameserver 10.96.0.10\n"+
		"search simplyblock.svc.acme.local svc.acme.local acme.local\n"+
		"options ndots:5\n")

	got, err := clusterDNSDomainFromResolvConf(path)
	if err != nil {
		t.Fatalf("clusterDNSDomainFromResolvConf: %v", err)
	}
	if got != "acme.local" {
		t.Fatalf("detected domain %q, want acme.local", got)
	}
}

// A pod on dnsPolicy: Default gets the node's resolver, which names no svc
// zone. Detection has to report that rather than invent a domain.
func TestAResolverWithoutAClusterSearchPathIsReported(t *testing.T) {
	path := writeResolvConf(t, "nameserver 1.1.1.1\nsearch example.com\n")

	if _, err := clusterDNSDomainFromResolvConf(path); err == nil {
		t.Fatal("a resolver with no svc entry was accepted")
	}
}

func TestDetectionFallsBackToTheDefaultDomain(t *testing.T) {
	t.Cleanup(func() { _ = SetClusterDNSDomain("") })

	domain, err := clusterDNSDomainFromResolvConf(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("a missing resolver file was accepted")
	}
	if domain != "" {
		t.Fatalf("a failed detection returned %q", domain)
	}
	if got := ClusterDNSDomain(); got != DefaultClusterDNSDomain {
		t.Fatalf("ClusterDNSDomain() = %q after a failed detection", got)
	}
}

// Regression: 2026-10-22-names-pinned-to-a-cluster-domain — an address carrying
// the cluster domain resolves only where that domain is the cluster's own. The
// name a client dials carries no domain, so the resolver's search list answers
// it on any cluster.
func TestTheStorageNodeAPIAddressCarriesNoClusterDomain(t *testing.T) {
	useClusterDNSDomain(t, "acme.local")

	got := StorageNodeSetAPIAddress("worker-1", "simplyblock", atlaskube.ConstantWorkloadNames())
	want := "worker-1.simplyblock-storage-node-api.simplyblock.svc:5000"
	if got != want {
		t.Fatalf("StorageNodeSetAPIAddress = %q, want %q", got, want)
	}
}

// The certificate is the one place that still names the domain: a name is only
// on it if it was issued for it, and both spellings are dialed.
func TestTheServingCertificateNamesBothSpellings(t *testing.T) {
	useClusterDNSDomain(t, "acme.local")

	cert := BuildServiceServingCertificate("simplyblock", "webapi", "webapi-tls")
	raw, _ := cert.Object["spec"].(map[string]any)["dnsNames"].([]any)
	names := make([]string, 0, len(raw))
	for _, n := range raw {
		names = append(names, n.(string))
	}

	want := []string{
		"webapi.simplyblock.svc",
		"*.webapi.simplyblock.svc",
		"webapi.simplyblock.svc.acme.local",
		"*.webapi.simplyblock.svc.acme.local",
	}
	for _, name := range want {
		if !slices.Contains(names, name) {
			t.Errorf("dnsNames %v lack %q", names, name)
		}
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".cluster.local") {
			t.Errorf("dnsNames carry %q on a cluster whose domain is acme.local", n)
		}
	}
}
