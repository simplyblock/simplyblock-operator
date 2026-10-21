// A cluster's workload names: the two headless Services its storage nodes are
// reached through, their serving-certificate Secrets, and the suffix a worker's
// hostname label carries in both. They live here because the control plane
// builds every node's hostname from them, so the operator that creates the
// Services and anything that predicts a node's address have to derive the same
// names, and a second formula would let them disagree.

package kube

import (
	"regexp"
	"strings"
)

// The constant names. The control plane uses them for a cluster whose record
// names none, so they are part of the identity of every node of a cluster
// created before the names were recorded.
const (
	ConstantNodeAPIService   = "simplyblock-storage-node-api"
	ConstantSpdkProxyService = "simplyblock-spdk-proxy"
)

// tlsSecretSuffix turns a Service's name into its serving-certificate Secret's.
const tlsSecretSuffix = "-tls"

// The formulas a cluster's own Services are named by. A Service name is one
// segment of every node's hostname, so it is held to a DNS label.
var (
	derivedNodeAPIService   = Formula{Kind: DNSLabel, Prefix: "sb-", Suffix: "-node-api"}
	derivedSpdkProxyService = Formula{Kind: DNSLabel, Prefix: "sb-", Suffix: "-spdk-proxy"}
)

// maxHostnameLength is what one DNS label, and so a hostname in a headless
// Service, may carry.
const maxHostnameLength = MaxLabelValueLength

// WorkloadNames are the names one cluster's workload is reached by.
type WorkloadNames struct {
	NodeAPIService     string
	NodeAPITLSSecret   string
	SpdkProxyService   string
	SpdkProxyTLSSecret string

	// NodeLabelSuffix is appended to a worker's hostname label in both
	// Services. Empty for every cluster but a supercluster's member.
	NodeLabelSuffix string
}

// ConstantWorkloadNames are the names a namespace's first cluster keeps, and
// the ones a cluster created before the names were recorded has.
func ConstantWorkloadNames() WorkloadNames {
	return servedBy(ConstantNodeAPIService, ConstantSpdkProxyService)
}

// DerivedWorkloadNames are the names a new cluster takes. A cluster name too
// long for a Service is cut and carries a digest of the whole name, so two
// names sharing a prefix stay apart. A name the Service cannot carry unchanged,
// one with a dot or an upper-case letter, always carries the digest, because
// prod.fast and prod-fast would otherwise derive one Service.
func DerivedWorkloadNames(cluster string) WorkloadNames {
	nodeAPI, spdkProxy := derivedNodeAPIService, derivedSpdkProxyService
	if DNSLabel.sanitize(cluster) != cluster {
		nodeAPI.AlwaysDigest, spdkProxy.AlwaysDigest = true, true
	}
	return servedBy(nodeAPI.Derive(cluster).Value, spdkProxy.Derive(cluster).Value)
}

func servedBy(nodeAPIService, spdkProxyService string) WorkloadNames {
	return WorkloadNames{
		NodeAPIService:     nodeAPIService,
		NodeAPITLSSecret:   nodeAPIService + tlsSecretSuffix,
		SpdkProxyService:   spdkProxyService,
		SpdkProxyTLSSecret: spdkProxyService + tlsSecretSuffix,
	}
}

// IsConstant reports whether these are the constant names.
func (n WorkloadNames) IsConstant() bool {
	return n == ConstantWorkloadNames()
}

// Hostname is a worker's hostname label in this cluster's Services: the worker
// name up to its first dot, so a fully qualified node name stays one DNS
// segment, followed by the cluster's suffix. A label too long to take the
// suffix is cut and carries an eight-character digest of the whole label, so
// it stays one segment and two long workers sharing a prefix stay apart. The
// control plane builds the same hostname (sbcli StorageNode._k8s_service_host),
// so the rule is the same on both sides.
func (n WorkloadNames) Hostname(worker string) string {
	label, _, _ := strings.Cut(worker, ".")
	if len(label)+len(n.NodeLabelSuffix) <= maxHostnameLength {
		return label + n.NodeLabelSuffix
	}
	keep := maxHostnameLength - len(n.NodeLabelSuffix) - digestLength - 1
	return strings.TrimRight(label[:keep], "-") + "-" + digestOf([]string{label}) + n.NodeLabelSuffix
}

// MemberWorkloadNames are the names a supercluster's member is reached by: its
// supercluster's Services, and a suffix of its own on every hostname, because
// one worker may carry nodes of several members.
func MemberWorkloadNames(supercluster, member string) WorkloadNames {
	names := DerivedWorkloadNames(supercluster)
	names.NodeLabelSuffix = "-" + memberSuffix(member)
	return names
}

// drawnSuffix matches the three letters an expansion draws onto a member's
// name, <cluster>-<nodeset>-<suffix>.
var drawnSuffix = regexp.MustCompile(`-([a-z]{3})$`)

// memberSuffix is the member's drawn suffix when its name carries one, and
// three characters of a digest of its name for a member written by hand.
func memberSuffix(member string) string {
	if match := drawnSuffix.FindStringSubmatch(member); match != nil {
		return match[1]
	}
	return digestOf([]string{member})[:memberSuffixLength]
}

// memberSuffixLength is the length of a member's suffix without its dash.
const memberSuffixLength = 3
