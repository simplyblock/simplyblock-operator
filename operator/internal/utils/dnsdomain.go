// In-cluster DNS names.
//
// Every name the operator hands to a client is <service>.<namespace>.svc, with
// no cluster domain on the end. A pod's resolver carries
// `search <ns>.svc.<domain> svc.<domain> <domain>` with ndots:5, so a two-dot
// name goes through the search list and the third suffix answers it. That
// works whatever domain the cluster was installed with, where a name that
// spells the domain out resolves only on a cluster using that spelling.
//
// Two callers still need the domain itself:
//
//   - Certificate SANs. A name is only on a certificate if it was issued for
//     it, so the issued set covers both spellings. The fully qualified one is
//     what a peer that has not restarted yet still dials.
//   - The FoundationDB cluster. Its operator builds coordinator names as
//     <pod>.<service>.<namespace>.svc.<dnsDomain> and its CRD rejects an empty
//     dnsDomain (minLength: 1), so it has to be told the real domain.
//
// DetectClusterDNSDomain reads it off the pod's own resolver, so neither
// caller needs it configured.
package utils

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/util/validation"
)

// DefaultClusterDNSDomain is the domain Kubernetes installs with unless told
// otherwise, and the fallback when the resolver says nothing useful.
const DefaultClusterDNSDomain = "cluster.local"

// resolvConfPath is where the kubelet writes the pod's resolver configuration.
const resolvConfPath = "/etc/resolv.conf"

var clusterDNSDomain atomic.Pointer[string]

// SetClusterDNSDomain overrides the cluster DNS domain for the process. A
// trailing dot is dropped, and an empty value restores the default. It refuses a
// value that is not a DNS subdomain, because every name built from it would be
// unresolvable.
func SetClusterDNSDomain(domain string) error {
	domain = strings.TrimSuffix(strings.TrimSpace(domain), ".")
	if domain == "" {
		domain = DefaultClusterDNSDomain
	}
	if errs := validation.IsDNS1123Subdomain(domain); len(errs) > 0 {
		return fmt.Errorf("cluster DNS domain %q: %s", domain, strings.Join(errs, "; "))
	}
	clusterDNSDomain.Store(&domain)
	return nil
}

// ClusterDNSDomain returns the cluster DNS domain, or the default when nothing
// has been detected or set.
func ClusterDNSDomain() string {
	if domain := clusterDNSDomain.Load(); domain != nil {
		return *domain
	}
	return DefaultClusterDNSDomain
}

// DetectClusterDNSDomain reads the domain out of the pod's resolver and stores
// it for the process.
//
// It never fails the caller. A pod running with dnsPolicy: Default gets the
// node's resolver, which carries no cluster search path at all, and the
// operator still has to come up. The returned domain is the default in that
// case, and the error describes why, for the caller to log.
func DetectClusterDNSDomain() (string, error) {
	domain, err := clusterDNSDomainFromResolvConf(resolvConfPath)
	if err != nil {
		return DefaultClusterDNSDomain, err
	}
	if err := SetClusterDNSDomain(domain); err != nil {
		return DefaultClusterDNSDomain, err
	}
	return ClusterDNSDomain(), nil
}

// clusterDNSDomainFromResolvConf picks the domain out of a resolver search list.
//
// The kubelet writes `search <ns>.svc.<domain> svc.<domain> <domain>`, and the
// middle entry is the one that identifies the domain without having to know the
// namespace: it is the only one of the three that starts with the svc label.
func clusterDNSDomainFromResolvConf(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read the resolver configuration: %w", err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || (fields[0] != "search" && fields[0] != "domain") {
			continue
		}
		for _, entry := range fields[1:] {
			if domain, ok := strings.CutPrefix(strings.TrimSuffix(entry, "."), "svc."); ok {
				return domain, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read the resolver configuration: %w", err)
	}
	return "", fmt.Errorf("no svc.<domain> entry in the search list of %s", path)
}

// ServiceDNSName is the name clients dial a Service by:
// <service>.<namespace>.svc, resolved through the pod's search list.
func ServiceDNSName(service, namespace string) string {
	return fmt.Sprintf("%s.%s.svc", service, namespace)
}

// ServiceFQDN is the fully qualified name of a Service:
// <service>.<namespace>.svc.<cluster DNS domain>.
//
// Only certificate SANs and the FoundationDB cluster file should use this; what
// anything dials is ServiceDNSName.
func ServiceFQDN(service, namespace string) string {
	return fmt.Sprintf("%s.%s", ServiceDNSName(service, namespace), ClusterDNSDomain())
}

// ServiceCertificateNames is every name a certificate for a Service has to
// carry, shortest first.
//
// Both the suffix-free name and the fully qualified one are on it, plus a
// wildcard for each: a headless Service is dialed per pod, as
// <pod>.<service>.<namespace>.svc, and one wildcard label covers the pod.
func ServiceCertificateNames(service, namespace string) []string {
	short := ServiceDNSName(service, namespace)
	full := ServiceFQDN(service, namespace)
	return []string{
		service,
		fmt.Sprintf("%s.%s", service, namespace),
		short,
		"*." + short,
		full,
		"*." + full,
	}
}
