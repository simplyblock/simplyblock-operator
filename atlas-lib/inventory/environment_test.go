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
