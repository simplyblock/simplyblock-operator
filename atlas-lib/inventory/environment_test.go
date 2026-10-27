// Which distribution the detection concludes from which markers, and which
// marker wins when a cluster carries two.
//
// Every case below is a real combination. A Rancher-managed K3s cluster carries
// both distributions' markers at once, an OpenShift cluster imported into
// Rancher carries both of those, and none of that is a misconfiguration: a
// management layer and a distribution are different things that leave marks in
// the same places. The tests pin which one the answer is and that the other is
// still reported.

package inventory

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// node builds a worker carrying the labels, annotations, and node info a test
// is about, and nothing else.
func node(name string, opts ...func(*corev1.Node)) corev1.Node {
	n := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{},
			Annotations: map[string]string{},
		},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			KubeletVersion: "v1.31.4",
			OSImage:        "Ubuntu 24.04.1 LTS",
		}},
	}
	for _, opt := range opts {
		opt(&n)
	}
	return n
}

func labeled(key, value string) func(*corev1.Node) {
	return func(n *corev1.Node) { n.Labels[key] = value }
}

func annotated(key, value string) func(*corev1.Node) {
	return func(n *corev1.Node) { n.Annotations[key] = value }
}

func kubelet(version string) func(*corev1.Node) {
	return func(n *corev1.Node) { n.Status.NodeInfo.KubeletVersion = version }
}

func osImage(image string) func(*corev1.Node) {
	return func(n *corev1.Node) { n.Status.NodeInfo.OSImage = image }
}

// evidenceFor reports whether the environment recorded evidence for a
// distribution.
func evidenceFor(env Environment, dist Distribution) bool {
	return slices.ContainsFunc(env.Evidence, func(e Evidence) bool { return e.Distribution == dist })
}

func TestDetectEnvironmentRecognizesOpenShiftByItsAPIGroups(t *testing.T) {
	groups := []string{"apps", "config.openshift.io", "route.openshift.io", "security.openshift.io"}

	env := DetectEnvironment(groups, []corev1.Node{node("worker-1", osImage("Red Hat Enterprise Linux CoreOS 419"))})

	if env.Distribution != DistributionOpenShift {
		t.Errorf("concluded %q, want %q", env.Distribution, DistributionOpenShift)
	}
	if len(env.Evidence) == 0 {
		t.Fatal("concluded a distribution and recorded no evidence for it")
	}
	// A reviewer corrects a finding, and correcting one means seeing what it
	// rested on.
	found := false
	for _, e := range env.Evidence {
		if e.Distribution == DistributionOpenShift && e.Source == SourceAPIGroup {
			found = true
		}
	}
	if !found {
		t.Errorf("recorded %+v, and none of it names the API group that decided it", env.Evidence)
	}
}

func TestDetectEnvironmentRecognizesOpenShiftByANodeLabelAlone(t *testing.T) {
	// A cluster whose API groups could not be listed, on nodes running RHEL
	// rather than CoreOS, still carries the label the installer put there. It
	// is the weakest of the four sources and it is read for exactly this case.
	env := DetectEnvironment(nil, []corev1.Node{
		node("worker-1", labeled("node.openshift.io/os_id", "rhel")),
	})

	if env.Distribution != DistributionOpenShift {
		t.Errorf("concluded %q, want %q", env.Distribution, DistributionOpenShift)
	}
	if len(env.Evidence) != 1 || env.Evidence[0].Source != SourceNodeLabel {
		t.Errorf("recorded %+v, want the one node label that decided it", env.Evidence)
	}
}

func TestDetectEnvironmentRecognizesTalosByItsNodeImage(t *testing.T) {
	env := DetectEnvironment(nil, []corev1.Node{node("worker-1", osImage("Talos (v1.9.2)"))})

	if env.Distribution != DistributionTalos {
		t.Errorf("concluded %q, want %q", env.Distribution, DistributionTalos)
	}
}

func TestDetectEnvironmentRecognizesTalosByItsAnnotations(t *testing.T) {
	// A Talos node whose image string a release changed still carries the
	// annotations the machine config writes, which is why more than one marker
	// is read for every distribution.
	env := DetectEnvironment(nil, []corev1.Node{
		node("worker-1", annotated("talos.dev/owned-labels", "[]")),
	})

	if env.Distribution != DistributionTalos {
		t.Errorf("concluded %q, want %q", env.Distribution, DistributionTalos)
	}
}

func TestDetectEnvironmentRecognizesK3sByItsKubeletVersion(t *testing.T) {
	env := DetectEnvironment(nil, []corev1.Node{node("worker-1", kubelet("v1.31.4+k3s1"))})

	if env.Distribution != DistributionK3s {
		t.Errorf("concluded %q, want %q", env.Distribution, DistributionK3s)
	}
}

func TestDetectEnvironmentReadsRKE2AsRancher(t *testing.T) {
	// Rancher is the enum's name for the whole family: RKE2 is a distribution
	// in its own right and there is no separate value for it, so its marker
	// resolves here.
	env := DetectEnvironment(nil, []corev1.Node{node("worker-1", kubelet("v1.31.4+rke2r1"))})

	if env.Distribution != DistributionRancher {
		t.Errorf("concluded %q, want %q", env.Distribution, DistributionRancher)
	}
}

func TestDetectEnvironmentRecognizesRancherManagement(t *testing.T) {
	env := DetectEnvironment(
		[]string{"apps", "management.cattle.io"},
		[]corev1.Node{node("worker-1", annotated("rke.cattle.io/machine", "abc"))},
	)

	if env.Distribution != DistributionRancher {
		t.Errorf("concluded %q, want %q", env.Distribution, DistributionRancher)
	}
}

func TestDetectEnvironmentPrefersTheDistributionOverTheManagementLayer(t *testing.T) {
	// A Rancher-managed K3s cluster carries both sets of markers. The host
	// assumptions a storage node is built against come from the distribution
	// that installed the kubelet, so K3s is the answer, and the Rancher
	// evidence stays on the record because a reviewer may want the other one.
	env := DetectEnvironment(
		[]string{"management.cattle.io"},
		[]corev1.Node{node("worker-1",
			kubelet("v1.31.4+k3s1"),
			annotated("management.cattle.io/pod-requests", "{}"),
		)},
	)

	if env.Distribution != DistributionK3s {
		t.Errorf("concluded %q, want %q", env.Distribution, DistributionK3s)
	}
	if !evidenceFor(env, DistributionRancher) {
		t.Errorf("recorded %+v, and the Rancher markers are missing from it", env.Evidence)
	}
}

func TestDetectEnvironmentPrefersOpenShiftOverEverything(t *testing.T) {
	// An OpenShift cluster imported into Rancher carries both, and OpenShift is
	// the one that changes what a storage node needs: the SCCs, the kubelet
	// this product does not reconfigure, and the host assumptions of CoreOS.
	env := DetectEnvironment(
		[]string{"config.openshift.io", "management.cattle.io"},
		[]corev1.Node{node("worker-1",
			osImage("Red Hat Enterprise Linux CoreOS 419"),
			annotated("management.cattle.io/pod-requests", "{}"),
		)},
	)

	if env.Distribution != DistributionOpenShift {
		t.Errorf("concluded %q, want %q", env.Distribution, DistributionOpenShift)
	}
	if !evidenceFor(env, DistributionRancher) {
		t.Errorf("recorded %+v, and the Rancher markers are missing from it", env.Evidence)
	}
}

func TestDetectEnvironmentConcludesVanillaWhenNothingIsDistinctive(t *testing.T) {
	env := DetectEnvironment([]string{"apps", "batch"}, []corev1.Node{node("worker-1"), node("worker-2")})

	if env.Distribution != DistributionVanilla {
		t.Errorf("concluded %q, want %q", env.Distribution, DistributionVanilla)
	}
	if len(env.Evidence) != 0 {
		t.Errorf("recorded %+v for a cluster with no distinctive marker; Vanilla is "+
			"the absence of evidence rather than evidence of its own", env.Evidence)
	}
}

func TestDetectEnvironmentConcludesVanillaWithNothingToReadAtAll(t *testing.T) {
	// A caller that could read neither the API groups nor the nodes gets an
	// answer rather than a failure, because the field it fills in is a starting
	// point a reviewer corrects.
	env := DetectEnvironment(nil, nil)

	if env.Distribution != DistributionVanilla {
		t.Errorf("concluded %q, want %q", env.Distribution, DistributionVanilla)
	}
}

func TestDetectEnvironmentEvidenceIsOrdered(t *testing.T) {
	// Two runs against one cluster write the same document, which is what makes
	// a re-run after an expansion readable beside the first.
	groups := []string{"management.cattle.io", "config.openshift.io"}
	nodes := []corev1.Node{
		node("worker-2", osImage("Talos (v1.9.2)")),
		node("worker-1", kubelet("v1.31.4+k3s1")),
	}

	first := DetectEnvironment(groups, nodes)
	second := DetectEnvironment(slices.Clone(groups), slices.Clone(nodes))

	if len(first.Evidence) != len(second.Evidence) {
		t.Fatalf("two runs recorded %d and %d findings", len(first.Evidence), len(second.Evidence))
	}
	for i := range first.Evidence {
		if first.Evidence[i] != second.Evidence[i] {
			t.Errorf("finding %d differs between two runs: %+v and %+v",
				i, first.Evidence[i], second.Evidence[i])
		}
	}
}

func TestDetectEnvironmentReadsEveryNodeAndNotOnlyTheFirst(t *testing.T) {
	// A fleet mid-migration has nodes of two shapes, and the distribution is a
	// property of the cluster rather than of whichever node was listed first.
	env := DetectEnvironment(nil, []corev1.Node{
		node("worker-1"),
		node("worker-2", kubelet("v1.31.4+k3s1")),
	})

	if env.Distribution != DistributionK3s {
		t.Errorf("concluded %q from a fleet whose second node carries the marker, want %q",
			env.Distribution, DistributionK3s)
	}
}

func TestDetectEnvironmentRecognizesTheManagedServices(t *testing.T) {
	// Each managed service is recognized from what a worker it provisioned
	// carries, and from one marker alone: a self-managed node group has no
	// node-pool label, and a node pool labeled by hand may carry no other.
	cases := []struct {
		name   string
		worker corev1.Node
		want   Distribution
	}{
		{"EKS by its kubelet version", node("worker-1", kubelet("v1.30.8-eks-2d5f260")), DistributionEKS},
		{"EKS by its node-group label", node("worker-1", labeled("eks.amazonaws.com/nodegroup", "storage")), DistributionEKS},
		{"GKE by its kubelet version", node("worker-1", kubelet("v1.30.5-gke.1014001")), DistributionGKE},
		{"GKE by its node-pool label", node("worker-1", labeled("cloud.google.com/gke-nodepool", "storage")), DistributionGKE},
		{"AKS by its agent-pool label", node("worker-1", labeled("kubernetes.azure.com/agentpool", "storage")), DistributionAKS},
		{"AKS by its cluster label", node("worker-1", labeled("kubernetes.azure.com/cluster", "MC_rg_aks_westeurope")), DistributionAKS},
		{"OKE by its node-pool label", node("worker-1", labeled("oke.oraclecloud.com/pool.name", "storage")), DistributionOKE},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := DetectEnvironment(nil, []corev1.Node{c.worker})
			if env.Distribution != c.want {
				t.Errorf("concluded %q, want %q", env.Distribution, c.want)
			}
		})
	}
}

func TestDetectEnvironmentIgnoresTheCloudProvidersOwnLabels(t *testing.T) {
	// A self-managed cluster on a cloud runs the provider's cloud controller
	// manager too, and that labels the nodes it registers. Those labels say
	// which cloud the machine is in, not who installed its kubelet. The AKS
	// keys are whole keys, so a longer key that starts with one is not AKS.
	env := DetectEnvironment(nil, []corev1.Node{node("worker-1",
		labeled("oci.oraclecloud.com/fault-domain", "FAULT-DOMAIN-1"),
		labeled("topology.k8s.aws/zone-id", "euc1-az2"),
		labeled("kubernetes.azure.com/managed", "false"),
		labeled("kubernetes.azure.com/cluster-name", "self-managed"),
		labeled("kubernetes.azure.com/agentpool-custom", "storage"),
	)})

	if env.Distribution != DistributionVanilla {
		t.Errorf("concluded %q from cloud-provider labels, want %q", env.Distribution, DistributionVanilla)
	}
}

func TestDetectEnvironmentPrefersTheManagedServiceOverRancher(t *testing.T) {
	// An EKS cluster imported into Rancher carries both. The managed service
	// installed the kubelet and owns its configuration, so it is the answer.
	env := DetectEnvironment(
		[]string{"management.cattle.io"},
		[]corev1.Node{node("worker-1",
			kubelet("v1.30.8-eks-2d5f260"),
			annotated("management.cattle.io/pod-requests", "{}"),
		)},
	)

	if env.Distribution != DistributionEKS {
		t.Errorf("concluded %q, want %q", env.Distribution, DistributionEKS)
	}
	if !evidenceFor(env, DistributionRancher) {
		t.Errorf("recorded %+v, and the Rancher markers are missing from it", env.Evidence)
	}
}

// A real GKE node, read from a one-node cluster on 2026-10-27 (COS, kernel
// 6.12, GKE 1.36) and trimmed to its labels, annotations, and node info. GKE
// registers no API group of its own, so the nodes are the whole of the case.
func TestARealGKENodeReadsAsGKE(t *testing.T) {
	raw, err := os.ReadFile("testdata/environment/gke-cos-n2.json")
	if err != nil {
		t.Fatal(err)
	}
	var list corev1.NodeList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}

	env := DetectEnvironment(nil, list.Items)

	if env.Distribution != DistributionGKE {
		t.Fatalf("distribution = %s, want GKE: %+v", env.Distribution, env.Evidence)
	}
	sources := map[Source]bool{}
	for _, e := range env.Evidence {
		if e.Distribution != DistributionGKE {
			t.Errorf("evidence for another distribution: %+v", e)
		}
		sources[e.Source] = true
	}
	if !sources[SourceKubeletVersion] || !sources[SourceNodeLabel] {
		t.Errorf("evidence came from %v, want both the kubelet version and the node labels", sources)
	}
}

// A real EKS node, read from a one-node cluster on 2026-10-27 (Amazon Linux
// 2023, kernel 6.18, EKS 1.37), trimmed like the GKE one. Like GKE, EKS
// registers no API group of its own.
func TestARealEKSNodeReadsAsEKS(t *testing.T) {
	raw, err := os.ReadFile("testdata/environment/eks-al2023-i4i.json")
	if err != nil {
		t.Fatal(err)
	}
	var list corev1.NodeList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}

	env := DetectEnvironment(nil, list.Items)

	if env.Distribution != DistributionEKS {
		t.Fatalf("distribution = %s, want EKS: %+v", env.Distribution, env.Evidence)
	}
	sources := map[Source]bool{}
	for _, e := range env.Evidence {
		if e.Distribution != DistributionEKS {
			t.Errorf("evidence for another distribution: %+v", e)
		}
		sources[e.Source] = true
	}
	if !sources[SourceKubeletVersion] || !sources[SourceNodeLabel] {
		t.Errorf("evidence came from %v, want both the kubelet version and the node labels", sources)
	}
}
