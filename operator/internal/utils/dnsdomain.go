// The cluster DNS domain: the suffix after <service>.<namespace>.svc that the
// cluster's resolver answers for. It is cluster.local unless the cluster was set
// up with a different one, and the operator learns it from the
// --cluster-dns-domain flag at startup.
//
// It lives here, as one process-wide value set once, because every name the
// operator builds for an in-cluster address or puts on a certificate needs it,
// and those builders sit deep below the reconcilers that would otherwise have to
// carry it.
package utils

import (
	"fmt"
	"strings"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/util/validation"
)

// DefaultClusterDNSDomain is the domain Kubernetes installs with unless told
// otherwise.
const DefaultClusterDNSDomain = "cluster.local"

var clusterDNSDomain atomic.Pointer[string]

// SetClusterDNSDomain sets the cluster DNS domain for the process. A trailing dot
// is dropped, and an empty value means the default. It refuses a value that is
// not a DNS subdomain, because every name built from it would be unresolvable.
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

// ClusterDNSDomain returns the cluster DNS domain, or the default when none was
// set.
func ClusterDNSDomain() string {
	if domain := clusterDNSDomain.Load(); domain != nil {
		return *domain
	}
	return DefaultClusterDNSDomain
}

// ServiceFQDN is the fully qualified name of a Service:
// <service>.<namespace>.svc.<cluster DNS domain>.
func ServiceFQDN(service, namespace string) string {
	return fmt.Sprintf("%s.%s.svc.%s", service, namespace, ClusterDNSDomain())
}
