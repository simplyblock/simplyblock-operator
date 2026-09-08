// Which Kubernetes distribution a cluster is, concluded from the marks its
// installer left.
//
// It lives in this package rather than beside the other Kubernetes correlation
// in atlas/kube because of what it is for. What a distribution decides is the
// host: whether the kubelet may be reconfigured, whether CPU topology is
// readable, where the kubelet keeps its directories, and which security context
// a privileged workload needs. It is read from the Kubernetes API and it is a
// fact about the machines, so it belongs with the rest of what a deployment is
// planned against rather than with the mapping from a volume to its objects.
//
// Detection reads several markers per distribution rather than one, because
// every single marker is the one that a release changes. An API group is the
// strongest evidence there is, since only its own installer registers it; a
// kubelet version suffix is next, since the distribution builds the binary; and
// a label or an annotation is the weakest, since anything can write one. All of
// them are read and all of them are recorded.
//
// Nothing here fails. The distribution is a starting point a reviewer corrects,
// so a cluster whose markers say nothing is vanilla Kubernetes rather than an
// error, and a caller that could read only half the evidence gets an answer
// from that half.

package inventory

import (
	"cmp"
	"context"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/discovery"
)

// Distribution is the Kubernetes distribution a cluster runs.
//
// The values are the distributions' own names, and they are the values the
// operator's KubernetesEnvironment enum carries, so that what is detected here
// is written into a deployment config without a translation table in between.
type Distribution string

const (
	// DistributionVanilla is upstream Kubernetes, and any distribution that
	// left no mark this recognizes. It is the answer when nothing else was
	// concluded, which is why it is also the least informative one.
	DistributionVanilla Distribution = "Vanilla"

	// DistributionOpenShift is Red Hat OpenShift, on RHCOS or RHEL.
	DistributionOpenShift Distribution = "OpenShift"

	// DistributionRancher is the Rancher family: RKE2, RKE, and a cluster of
	// any provenance that Rancher manages. There is no separate value for RKE2,
	// so its marker resolves here.
	DistributionRancher Distribution = "Rancher"

	// DistributionK3s is K3s, whose single binary keeps the kubelet's state
	// somewhere a vanilla assumption does not find it.
	DistributionK3s Distribution = "K3s"

	// DistributionTalos is Talos Linux, whose host has no shell, an immutable
	// root filesystem, and a kubelet configured only through its machine
	// config.
	DistributionTalos Distribution = "Talos"
)

// Source is where one finding was read, which is how strong it is.
type Source string

const (
	// SourceAPIGroup is an API group only one distribution's installer
	// registers, and is the strongest evidence available.
	SourceAPIGroup Source = "APIGroup"

	// SourceKubeletVersion is the version suffix a distribution builds into the
	// kubelet it ships.
	SourceKubeletVersion Source = "KubeletVersion"

	// SourceOSImage is the operating-system image a node boots, as the kubelet
	// reports it.
	SourceOSImage Source = "OSImage"

	// SourceNodeLabel and SourceNodeAnnotation are marks an installer put on a
	// node. They are the weakest evidence, because anything with write access
	// to a node can leave one.
	SourceNodeLabel      Source = "NodeLabel"
	SourceNodeAnnotation Source = "NodeAnnotation"
)

// Evidence is one marker that was found, and what it points at.
type Evidence struct {
	// Distribution is what the marker points at.
	Distribution Distribution

	// Source is where it was read.
	Source Source

	// Detail is the marker itself: the group's name, the version string, the
	// label's key and value. It is what a reviewer reads to decide whether the
	// conclusion was right.
	Detail string
}

// Environment is what a cluster was concluded to be.
type Environment struct {
	// Distribution is the conclusion, and is never empty: a cluster with no
	// recognized marker is DistributionVanilla.
	Distribution Distribution

	// Evidence is every marker that was found, for every distribution, ordered
	// so that two runs against one cluster record it identically. It is empty
	// for a vanilla cluster, because vanilla is concluded from the absence of
	// markers rather than from any of its own.
	Evidence []Evidence
}

// precedence is the order a conclusion is drawn in when a cluster carries more
// than one distribution's markers, most decisive first.
//
// It is not a ranking of how much evidence there is. OpenShift is first because
// it changes the most about what a workload on the host may do, and it is
// unambiguous: nothing else registers its API groups. Talos is next for the same
// reason from the other direction, since its host is the most constrained one
// this product runs on.
//
// Rancher is last of the four, and that is the one entry worth stating a reason
// for. It is a management layer as much as a distribution, so a Rancher-managed
// K3s cluster carries both sets of markers. What decides the host's shape is the
// distribution that installed the kubelet, so K3s wins, and the Rancher finding
// stays on the record for a reviewer who wants the other answer.
var precedence = []Distribution{
	DistributionOpenShift,
	DistributionTalos,
	DistributionK3s,
	DistributionRancher,
}

// apiGroupMarkers are the API groups that identify a distribution outright.
var apiGroupMarkers = map[string]Distribution{
	"config.openshift.io":               DistributionOpenShift,
	"operator.openshift.io":             DistributionOpenShift,
	"route.openshift.io":                DistributionOpenShift,
	"security.openshift.io":             DistributionOpenShift,
	"machineconfiguration.openshift.io": DistributionOpenShift,
	"management.cattle.io":              DistributionRancher,
	"provisioning.cattle.io":            DistributionRancher,
	"rke.cattle.io":                     DistributionRancher,
	"k3s.cattle.io":                     DistributionK3s,
	"talos.dev":                         DistributionTalos,
}

// kubeletVersionMarkers are the suffixes a distribution builds into its
// kubelet's version string, such as v1.31.4+k3s1 and v1.31.4+rke2r1.
var kubeletVersionMarkers = map[string]Distribution{
	"+k3s":  DistributionK3s,
	"+rke2": DistributionRancher,
	"+rke":  DistributionRancher,
}

// osImageMarkers are prefixes of the image string a node's kubelet reports.
//
// They are prefixes and not substrings on purpose: "Talos" appears in the image
// of anything built on it, and what this is asking is what the node boots.
var osImageMarkers = map[string]Distribution{
	"Talos":                           DistributionTalos,
	"Red Hat Enterprise Linux CoreOS": DistributionOpenShift,
}

// nodeKeyMarkers are the label and annotation key prefixes an installer leaves
// on the nodes it created.
var nodeKeyMarkers = map[string]Distribution{
	"node.openshift.io/":                 DistributionOpenShift,
	"machineconfiguration.openshift.io/": DistributionOpenShift,
	"talos.dev/":                         DistributionTalos,
	"k3s.io/":                            DistributionK3s,
	"rke2.io/":                           DistributionRancher,
	"rke.cattle.io/":                     DistributionRancher,
	"management.cattle.io/":              DistributionRancher,
	"cattle.io/":                         DistributionRancher,
}

// CollectEnvironment reads the API groups the server registers and concludes a
// distribution from them and from the nodes it was given.
//
// The two halves are separate calls because they fail separately and because
// the nodes are almost always already at hand: a caller inspecting a fleet has
// listed them for other reasons. An unreadable group list is not fatal — the
// nodes alone still support a conclusion — so the error is returned beside an
// Environment that was drawn from whatever was readable.
func CollectEnvironment(ctx context.Context, disco discovery.DiscoveryInterface, nodes []corev1.Node) (Environment, error) {
	groups, err := serverGroups(ctx, disco)
	return DetectEnvironment(groups, nodes), err
}

// serverGroups lists the API groups the server registers, by name.
func serverGroups(ctx context.Context, disco discovery.DiscoveryInterface) ([]string, error) {
	if disco == nil {
		return nil, nil
	}
	// ServerGroups takes no context, so the cancellation this call has to
	// respect is checked around it rather than inside it.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	list, err := disco.ServerGroups()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Groups))
	for _, group := range list.Groups {
		names = append(names, group.Name)
	}
	return names, nil
}

// DetectEnvironment concludes a distribution from the API groups a server
// registers and the nodes it runs.
//
// Both arguments are optional. A caller with only one of the two gets the
// conclusion that half supports, and a caller with neither gets
// DistributionVanilla, because the field this fills in is a starting point a
// reviewer corrects rather than a question that has to be answered here.
func DetectEnvironment(apiGroups []string, nodes []corev1.Node) Environment {
	var evidence []Evidence

	for _, group := range apiGroups {
		if dist, ok := apiGroupMarkers[group]; ok {
			evidence = append(evidence, Evidence{dist, SourceAPIGroup, group})
		}
	}

	for _, n := range nodes {
		evidence = append(evidence, nodeEvidence(n)...)
	}

	env := Environment{Distribution: DistributionVanilla, Evidence: dedupe(evidence)}
	for _, dist := range precedence {
		if slices.ContainsFunc(env.Evidence, func(e Evidence) bool { return e.Distribution == dist }) {
			env.Distribution = dist
			break
		}
	}
	return env
}

// nodeEvidence reads one node's four markers.
func nodeEvidence(n corev1.Node) []Evidence {
	var evidence []Evidence

	for suffix, dist := range kubeletVersionMarkers {
		if strings.Contains(n.Status.NodeInfo.KubeletVersion, suffix) {
			evidence = append(evidence, Evidence{dist, SourceKubeletVersion, n.Status.NodeInfo.KubeletVersion})
		}
	}
	for prefix, dist := range osImageMarkers {
		if strings.HasPrefix(n.Status.NodeInfo.OSImage, prefix) {
			evidence = append(evidence, Evidence{dist, SourceOSImage, n.Status.NodeInfo.OSImage})
		}
	}
	for key := range n.Labels {
		if dist, ok := markerFor(key); ok {
			evidence = append(evidence, Evidence{dist, SourceNodeLabel, key})
		}
	}
	for key := range n.Annotations {
		if dist, ok := markerFor(key); ok {
			evidence = append(evidence, Evidence{dist, SourceNodeAnnotation, key})
		}
	}
	return evidence
}

// markerFor matches a label or annotation key against the key prefixes, longest
// prefix first: cattle.io/ and management.cattle.io/ both point at Rancher
// today, and a shorter prefix must not shadow a longer one that could point
// somewhere else tomorrow.
func markerFor(key string) (Distribution, bool) {
	for _, prefix := range nodeKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return nodeKeyMarkers[prefix], true
		}
	}
	return "", false
}

// nodeKeyPrefixes is nodeKeyMarkers' keys, longest first, computed once because
// markerFor runs for every label and annotation of every node of a fleet.
var nodeKeyPrefixes = func() []string {
	prefixes := make([]string, 0, len(nodeKeyMarkers))
	for prefix := range nodeKeyMarkers {
		prefixes = append(prefixes, prefix)
	}
	slices.SortFunc(prefixes, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), cmp.Compare(a, b))
	})
	return prefixes
}()

// dedupe orders the findings and drops the repeats.
//
// A fleet of twenty identical nodes yields the same finding twenty times, which
// says nothing the first one did not, and the order the maps and the node list
// were walked in is not an order at all. Sorting and compacting makes two runs
// against one cluster record the same evidence, which is what lets a reviewer
// read a re-run beside the document it grew from.
func dedupe(evidence []Evidence) []Evidence {
	if len(evidence) == 0 {
		return nil
	}
	slices.SortFunc(evidence, func(a, b Evidence) int {
		return cmp.Or(
			slices.Index(precedence, a.Distribution)-slices.Index(precedence, b.Distribution),
			cmp.Compare(a.Source, b.Source),
			cmp.Compare(a.Detail, b.Detail),
		)
	})
	return slices.Compact(evidence)
}
