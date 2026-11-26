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
// kubelet or API server version suffix is next, since the distribution builds
// the binary; and a label or an annotation is the weakest, since anything can
// write one. All of them are read and all of them are recorded.
//
// Nothing here fails. The distribution is a starting point a reviewer corrects,
// so a cluster whose markers say nothing is vanilla Kubernetes rather than an
// error, and a caller that could read only half the evidence gets an answer
// from that half.

package inventory

import (
	"cmp"
	"context"
	"errors"
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

	// DistributionEKS, DistributionGKE, DistributionAKS, and DistributionOKE are
	// the managed services of Amazon, Google, Microsoft, and Oracle. The
	// provider builds the node image and the kubelet configuration and replaces
	// both whenever a node pool is upgraded or scaled, so a change a workload
	// makes to either is one the next node does not have.
	DistributionEKS Distribution = "EKS"
	DistributionGKE Distribution = "GKE"
	DistributionAKS Distribution = "AKS"
	DistributionOKE Distribution = "OKE"
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

	// SourceServerVersion is the same suffix in the API server's version, the
	// one marker a managed control plane leaves: it runs no node a cluster can
	// list.
	SourceServerVersion Source = "ServerVersion"

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

// ControlPlane is what the API server says about the cluster it serves.
type ControlPlane struct {
	// APIGroups are the API groups the server registers, by name.
	APIGroups []string

	// Version is the server's own build, as /version reports it in gitVersion.
	Version string
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
// The managed services come after Talos and before K3s and Rancher. None of them
// carries another's markers, so their order among themselves decides nothing.
// They precede Rancher because Rancher imports clusters it did not install: an
// EKS cluster under Rancher management carries both, and EKS built the kubelet.
//
// Rancher is last, and that is the one entry worth stating a reason
// for. It is a management layer as much as a distribution, so a Rancher-managed
// K3s cluster carries both sets of markers. What decides the host's shape is the
// distribution that installed the kubelet, so K3s wins, and the Rancher finding
// stays on the record for a reviewer who wants the other answer.
//
// RKE2 is the same pair the other way around, and the order alone gets it wrong:
// see kubeletDecidesBetweenK3sAndRancher.
var precedence = []Distribution{
	DistributionOpenShift,
	DistributionTalos,
	DistributionEKS,
	DistributionGKE,
	DistributionAKS,
	DistributionOKE,
	DistributionK3s,
	DistributionRancher,
}

// apiGroupMarkers are the API groups that identify a distribution outright.
//
// k3s.cattle.io is the exception. RKE2 is built on the K3s code and serves the
// group as well, for its add-ons and its etcd snapshot records, so the group
// says "K3s or RKE2" and the kubelet says which
// (kubeletDecidesBetweenK3sAndRancher).
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
// kubelet's version string, such as `v1.31.4+k3s1`, `v1.31.4+rke2r1`,
// `v1.30.8-eks-2d5f260`, and `v1.30.5-gke.1014001`.
//
// The two managed-service suffixes are the build's and not the service's. EKS
// Distro, which EKS Anywhere runs, carries `-eks-` as well, and Google
// Distributed Cloud carries `-gke.`, so those clusters read as EKS and GKE. The
// conclusion then leaves the kubelet configuration alone on a host whose
// kubelet could have been configured, which a reviewer corrects in the draft.
var kubeletVersionMarkers = map[string]Distribution{
	"+k3s":  DistributionK3s,
	"+rke2": DistributionRancher,
	"+rke":  DistributionRancher,
	"-eks-": DistributionEKS,
	"-gke.": DistributionGKE,
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
//
// The managed-service entries are the labels the service puts on the nodes of
// its own node pools. The cloud controller manager's labels are not among them
// (oci.oraclecloud.com/, topology.k8s.aws/, and kubernetes.azure.com/managed),
// because a self-managed cluster on the same cloud runs that controller too.
// AKS shares its prefix with the Azure cloud controller manager, so it is
// matched by whole keys in nodeKeyExactMarkers instead.
var nodeKeyMarkers = map[string]Distribution{
	"node.openshift.io/":                 DistributionOpenShift,
	"machineconfiguration.openshift.io/": DistributionOpenShift,
	"talos.dev/":                         DistributionTalos,
	"k3s.io/":                            DistributionK3s,
	"rke2.io/":                           DistributionRancher,
	"rke.cattle.io/":                     DistributionRancher,
	"management.cattle.io/":              DistributionRancher,
	"cattle.io/":                         DistributionRancher,
	"eks.amazonaws.com/":                 DistributionEKS,
	"cloud.google.com/gke-":              DistributionGKE,
	"oke.oraclecloud.com/":               DistributionOKE,
}

// nodeKeyExactMarkers are label and annotation keys that identify a
// distribution only when they match whole.
var nodeKeyExactMarkers = map[string]Distribution{
	"kubernetes.azure.com/agentpool": DistributionAKS,
	"kubernetes.azure.com/cluster":   DistributionAKS,
}

// CollectEnvironment reads the API groups the server registers and the
// server's version, and concludes a distribution from them and from the nodes
// it was given.
//
// The two halves are separate calls because they fail separately and because
// the nodes are almost always already at hand: a caller inspecting a fleet has
// listed them for other reasons. An unreadable group list or version is not
// fatal, since the nodes alone still support a conclusion, so the error is
// returned beside an Environment drawn from whatever was readable.
func CollectEnvironment(ctx context.Context, disco discovery.DiscoveryInterface, nodes []corev1.Node) (Environment, error) {
	control, err := readControlPlane(ctx, disco)
	return DetectEnvironment(control, nodes), err
}

// readControlPlane reads the API groups the server registers, by name, and the
// server's version.
func readControlPlane(ctx context.Context, disco discovery.DiscoveryInterface) (ControlPlane, error) {
	var control ControlPlane
	if disco == nil {
		return control, nil
	}
	// Neither call takes a context, so the cancellation this has to respect is
	// checked around them rather than inside them.
	if err := ctx.Err(); err != nil {
		return control, err
	}
	var errs []error
	if list, err := disco.ServerGroups(); err != nil {
		errs = append(errs, err)
	} else {
		for _, group := range list.Groups {
			control.APIGroups = append(control.APIGroups, group.Name)
		}
	}
	if info, err := disco.ServerVersion(); err != nil {
		errs = append(errs, err)
	} else {
		control.Version = info.GitVersion
	}
	return control, errors.Join(errs...)
}

// DetectEnvironment concludes a distribution from what the API server says
// about itself and from the nodes it runs.
//
// Both arguments are optional. A caller with only one of the two gets the
// conclusion that half supports, and a caller with neither gets
// DistributionVanilla, because the field this fills in is a starting point a
// reviewer corrects rather than a question that has to be answered here.
//
// A fleet whose workers agree, or say nothing, is concluded from every marker
// at once by the precedence. A fleet whose workers disagree is one control plane
// with workers from more than one provider, such as Google Anthos joining EKS
// and OKE node groups to a GKE cluster, and there the precedence would name
// whichever provider comes first in it. The control plane is the cluster's
// answer instead, and the workers that differ from it are each concluded on
// their own by NodeDistribution. Where the control plane carries no marker, the
// precedence answers as before.
func DetectEnvironment(control ControlPlane, nodes []corev1.Node) Environment {
	own := controlPlaneEvidence(control)
	evidence := slices.Clone(own)

	agree := true
	var seen Distribution
	for _, n := range nodes {
		found := nodeEvidence(n)
		evidence = append(evidence, found...)
		if dist, ok := conclude(found); ok {
			if seen != "" && dist != seen {
				agree = false
			}
			seen = dist
		}
	}

	env := Environment{Distribution: DistributionVanilla, Evidence: dedupe(evidence)}
	if !agree {
		if dist, ok := conclude(own); ok {
			env.Distribution = dist
			return env
		}
	}
	if dist, ok := conclude(env.Evidence); ok {
		env.Distribution = dist
	}
	return env
}

// NodeDistribution concludes a distribution from one node's own markers, and
// reports false for a node that carries none. A node without a marker says
// nothing about itself, which is not the same as saying it runs vanilla
// Kubernetes, so a caller comparing one worker against its cluster leaves such
// a node with the cluster's answer.
func NodeDistribution(n corev1.Node) (Distribution, bool) {
	return conclude(nodeEvidence(n))
}

// conclude applies the precedence to a set of findings, and reports false when
// none of them points anywhere.
func conclude(evidence []Evidence) (Distribution, bool) {
	for _, dist := range precedence {
		if slices.ContainsFunc(evidence, func(e Evidence) bool { return e.Distribution == dist }) {
			return kubeletDecidesBetweenK3sAndRancher(dist, evidence), true
		}
	}
	return "", false
}

// controlPlaneEvidence reads the API server's markers: the groups only one
// distribution's installer registers, and the build suffix of its version,
// which a distribution that builds the kubelet builds into the API server too.
func controlPlaneEvidence(control ControlPlane) []Evidence {
	var evidence []Evidence
	for _, group := range control.APIGroups {
		if dist, ok := apiGroupMarkers[group]; ok {
			evidence = append(evidence, Evidence{dist, SourceAPIGroup, group})
		}
	}
	for suffix, dist := range kubeletVersionMarkers {
		if strings.Contains(control.Version, suffix) {
			evidence = append(evidence, Evidence{dist, SourceServerVersion, control.Version})
		}
	}
	return evidence
}

// kubeletDecidesBetweenK3sAndRancher turns a K3s conclusion into Rancher for a
// cluster whose kubelets were all built by the Rancher family.
//
// An RKE2 cluster carries a K3s marker of its own: it serves the k3s.cattle.io
// API group, because RKE2 is built on the K3s code. With K3s ahead of Rancher
// in the precedence, every RKE2 cluster was concluded to be K3s (RKE2
// v1.36.6+rke2r1, 2026-11-13). The precedence exists for a Rancher-managed K3s
// cluster, and the rule it states is that the distribution that installed the
// kubelet decides, so the kubelet's own build suffix is asked: where it names
// the Rancher family and no kubelet names K3s, the cluster is Rancher's.
//
// The API server's version counts as a kubelet's here, because the same build
// produced both, and it is the one version there is when a fleet of mixed
// workers is concluded from its control plane alone. A cluster with kubelets of
// both, or with no version to read, keeps the precedence's answer.
func kubeletDecidesBetweenK3sAndRancher(concluded Distribution, evidence []Evidence) Distribution {
	if concluded != DistributionK3s {
		return concluded
	}
	builtBy := func(dist Distribution) bool {
		return slices.ContainsFunc(evidence, func(e Evidence) bool {
			return e.Distribution == dist &&
				(e.Source == SourceKubeletVersion || e.Source == SourceServerVersion)
		})
	}
	if builtBy(DistributionRancher) && !builtBy(DistributionK3s) {
		return DistributionRancher
	}
	return concluded
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

// markerFor matches a label or annotation key against the whole keys first, then
// against the key prefixes, longest prefix first: cattle.io/ and management.cattle.io/ both point at Rancher
// today, and a shorter prefix must not shadow a longer one that could point
// somewhere else tomorrow.
func markerFor(key string) (Distribution, bool) {
	if dist, ok := nodeKeyExactMarkers[key]; ok {
		return dist, true
	}
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
