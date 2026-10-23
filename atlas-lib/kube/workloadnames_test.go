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

// A member is reached through its supercluster's Services, so that a volume
// moved between members and a host connected to several of them need nothing
// per member, with its own suffix on every hostname, because one worker may
// carry nodes of several members.
func TestAMemberIsReachedThroughItsSuperclustersServicesWithItsOwnSuffix(t *testing.T) {
	got := MemberWorkloadNames("prod", "prod-dense-kqv")

	want := DerivedWorkloadNames("prod")
	want.NodeLabelSuffix = "-kqv"
	if got != want {
		t.Errorf("MemberWorkloadNames = %+v, want %+v", got, want)
	}
}

// A member written by hand has no drawn suffix in its name. It takes three
// characters of a digest of its name, the same on every call.
func TestAHandWrittenMembersSuffixComesFromItsName(t *testing.T) {
	first, again := MemberWorkloadNames("prod", "fast-tier"), MemberWorkloadNames("prod", "fast-tier")
	other := MemberWorkloadNames("prod", "dense-tier")

	if len(first.NodeLabelSuffix) != 4 || first.NodeLabelSuffix[0] != '-' {
		t.Errorf("suffix = %q, want a dash and three characters", first.NodeLabelSuffix)
	}
	if first != again {
		t.Errorf("two calls derive %q and %q", first.NodeLabelSuffix, again.NodeLabelSuffix)
	}
	if first.NodeLabelSuffix == other.NodeLabelSuffix {
		t.Errorf("two members derive one suffix %q", first.NodeLabelSuffix)
	}
	if errs := validation.IsDNS1123Label("worker-1" + first.NodeLabelSuffix); len(errs) > 0 {
		t.Errorf("the suffix does not make a hostname label: %v", errs)
	}
}

// A dot in a cluster name becomes a hyphen in a Service name, so prod.fast and
// prod-fast would derive one Service, and the second cluster would be left
// without a workload. A name the Service cannot carry unchanged carries a
// digest of the whole name instead, and a name it can carry stays readable.
func TestClusterNamesThatSanitizeAlikeDeriveDifferentServices(t *testing.T) {
	dotted, hyphenated := DerivedWorkloadNames("prod.fast"), DerivedWorkloadNames("prod-fast")

	if dotted.NodeAPIService == hyphenated.NodeAPIService || dotted.SpdkProxyService == hyphenated.SpdkProxyService {
		t.Errorf("prod.fast and prod-fast derive %+v and %+v", dotted, hyphenated)
	}
	if hyphenated.NodeAPIService != "sb-prod-fast-node-api" {
		t.Errorf("prod-fast derives %q, want the readable sb-prod-fast-node-api", hyphenated.NodeAPIService)
	}
	if errs := validation.IsDNS1035Label(dotted.NodeAPIService); len(errs) > 0 {
		t.Errorf("prod.fast derives %q: %v", dotted.NodeAPIService, errs)
	}
}

// A worker label of up to 63 characters plus a member's suffix would exceed
// the 63 a DNS label may carry. The label is cut and carries a digest of the
// whole worker label, so it stays one valid segment, keeps the suffix, and two
// long workers sharing a prefix stay apart. sbcli builds the same hostname.
func TestALongWorkerLabelWithASuffixStaysOneDNSLabel(t *testing.T) {
	names := MemberWorkloadNames("prod", "prod-dense-kqv")
	long := strings.Repeat("w", 60)

	first, second := names.Hostname(long+"-one.example.com"), names.Hostname(long+"-two")

	for _, hostname := range []string{first, second} {
		if errs := validation.IsDNS1123Label(hostname); len(errs) > 0 {
			t.Errorf("hostname %q: %v", hostname, errs)
		}
		if !strings.HasSuffix(hostname, "-kqv") {
			t.Errorf("hostname %q lost the member's suffix", hostname)
		}
	}
	if first == second {
		t.Errorf("two long workers share hostname %q", first)
	}
	if want := long[:63-4-9] + "-" + digestOf([]string{long + "-one"}) + "-kqv"; first != want {
		t.Errorf("hostname = %q, want %q, the rule sbcli applies", first, want)
	}
}

// The control plane builds a member's node hostnames by the same rule,
// independently, and the two have to agree or the name the control plane
// dials is not the one published (design-superclusters.md U-24). These are
// sbcli's own answers, from StorageNode._k8s_service_host, for a short name, a
// name too long to take the suffix, and a worker name carrying a domain, of
// which the agent reports the short hostname.
func TestHostnamesAgreeWithTheControlPlanes(t *testing.T) {
	for _, tc := range []struct {
		worker, suffix, want string
	}{
		{"worker-2", "-cjd", "worker-2-cjd"},
		{"worker-node-in-rack-twelve-of-the-eu-west-datacenter-number-0007", "-kqv",
			"worker-node-in-rack-twelve-of-the-eu-west-datacent-48b897af-kqv"},
		{"worker-2.example.internal", "-cjd", "worker-2-cjd"},
	} {
		names := DerivedWorkloadNames("prod")
		names.NodeLabelSuffix = tc.suffix
		if got := names.Hostname(tc.worker); got != tc.want {
			t.Errorf("Hostname(%q) with suffix %q = %q, want the control plane's %q", tc.worker, tc.suffix, got, tc.want)
		}
	}
}
