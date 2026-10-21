// Tests for a cluster's workload names: the constant names a namespace's first
// cluster keeps, the names every other cluster derives from its own, and the
// hostname a worker has in either.

package kube

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestTheConstantNamesAreTheOnesTheControlPlaneDefaultsTo(t *testing.T) {
	got := ConstantWorkloadNames()

	want := WorkloadNames{
		NodeAPIService:     "simplyblock-storage-node-api",
		NodeAPITLSSecret:   "simplyblock-storage-node-api-tls",
		SpdkProxyService:   "simplyblock-spdk-proxy",
		SpdkProxyTLSSecret: "simplyblock-spdk-proxy-tls",
	}
	if got != want {
		t.Errorf("ConstantWorkloadNames() = %+v, want %+v", got, want)
	}
	if !got.IsConstant() {
		t.Error("the constant names do not report themselves constant")
	}
}

func TestAClustersOwnNamesAreDerivedFromItsName(t *testing.T) {
	got := DerivedWorkloadNames("prod")

	want := WorkloadNames{
		NodeAPIService:     "sb-prod-node-api",
		NodeAPITLSSecret:   "sb-prod-node-api-tls",
		SpdkProxyService:   "sb-prod-spdk-proxy",
		SpdkProxyTLSSecret: "sb-prod-spdk-proxy-tls",
	}
	if got != want {
		t.Errorf("DerivedWorkloadNames(prod) = %+v, want %+v", got, want)
	}
	if got.IsConstant() {
		t.Error("derived names report themselves constant")
	}
}

// A StorageCluster name may be 63 characters and may carry dots, and a Service
// name is a DNS label: neither may survive into it unchanged.
func TestADerivedServiceNameIsAValidServiceNameWhateverTheClusterName(t *testing.T) {
	for _, name := range []string{
		strings.Repeat("a", 63),
		"cluster.with.dots",
		strings.Repeat("x", 50) + ".tail",
	} {
		names := DerivedWorkloadNames(name)
		for _, service := range []string{names.NodeAPIService, names.SpdkProxyService} {
			if errs := validation.IsDNS1035Label(service); len(errs) > 0 {
				t.Errorf("cluster %q derives Service %q: %v", name, service, errs)
			}
		}
		for _, secret := range []string{names.NodeAPITLSSecret, names.SpdkProxyTLSSecret} {
			if errs := validation.IsDNS1123Subdomain(secret); len(errs) > 0 {
				t.Errorf("cluster %q derives Secret %q: %v", name, secret, errs)
			}
		}
	}
}

// Two long names that share their first characters must not be cut to one
// Service, or the second cluster would take over the first one's.
func TestTwoLongClusterNamesSharingAPrefixDeriveDifferentServices(t *testing.T) {
	prefix := strings.Repeat("a", 55)
	first, second := DerivedWorkloadNames(prefix+"-one"), DerivedWorkloadNames(prefix+"-two")

	if first.NodeAPIService == second.NodeAPIService {
		t.Errorf("both clusters derive %q", first.NodeAPIService)
	}
}

func TestAWorkersHostnameCarriesTheNodeLabelSuffix(t *testing.T) {
	names := DerivedWorkloadNames("prod")
	if got := names.Hostname("worker-1.example.com"); got != "worker-1" {
		t.Errorf("Hostname = %q, want worker-1", got)
	}

	names.NodeLabelSuffix = "-abc"
	if got := names.Hostname("worker-1.example.com"); got != "worker-1-abc" {
		t.Errorf("Hostname = %q, want worker-1-abc", got)
	}
}
