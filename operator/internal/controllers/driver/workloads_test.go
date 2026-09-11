// U-06 to U-09, U-33, U-34, U-42, and U-43: what reaches which plugin, and what
// must not reach the other.
//
// The pairs matter more than the individual rows. A field that reaches both
// plugins when it should reach one is a rolling restart of every node plugin in
// the cluster for a change somebody made to the controller.

package driver

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/simplyblock/atlas/ptr"
)

func containerNamed(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func argValue(c *corev1.Container, flag string) (string, bool) {
	for _, a := range c.Args {
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"="), true
		}
	}
	return "", false
}

// U-06: one image versions both plugins, and never two. The image is the
// resolved one rather than the spec's, so the driver a deployment that stated
// nothing runs is the same on both sides.
func TestOneImageReachesBothPlugins(t *testing.T) {
	const resolved = "public.ecr.aws/simply-block/spdkcsi:resolved-not-from-the-spec"

	d := testDriver("simplyblock")
	d.Spec.Image = "quay.io/simplyblock-io/spdkcsi:the-spec-says-something-else"

	node := containerNamed(nodeDaemonSet(d, resolved).Spec.Template.Spec.Containers, "csi-node")
	controller := containerNamed(
		controllerStatefulSet(d, resolved).Spec.Template.Spec.Containers, "csi-controller")
	if node == nil || controller == nil {
		t.Fatal("a plugin container is missing")
	}
	if node.Image != resolved || controller.Image != resolved {
		t.Errorf("node = %q, controller = %q, want both %q", node.Image, controller.Image, resolved)
	}
}

// U-42 and U-43: every sidecar is applied, and each is addressed to this
// driver's own socket rather than to whatever else the cluster runs.
func TestEverySidecarIsAppliedAndAddressedToThisDriver(t *testing.T) {
	d := testDriver("simplyblock")
	sts := controllerStatefulSet(d, testImage)

	want := []string{"csi-provisioner", "csi-snapshotter", "csi-attacher", "csi-resizer", "csi-health-monitor"}
	for _, name := range want {
		c := containerNamed(sts.Spec.Template.Spec.Containers, name)
		if c == nil {
			t.Errorf("%s is not applied", name)
			continue
		}
		addr, ok := argValue(c, "--csi-address")
		if !ok || addr != controllerSocketPath {
			t.Errorf("%s --csi-address = %q, want %q", name, addr, controllerSocketPath)
		}
	}

	registrar := containerNamed(nodeDaemonSet(d, testImage).Spec.Template.Spec.Containers, "csi-registrar")
	if registrar == nil {
		t.Fatal("csi-registrar is not applied")
	}
	if addr, _ := argValue(registrar, "--csi-address"); addr != nodeSocketPath {
		t.Errorf("csi-registrar --csi-address = %q, want the node socket %q", addr, nodeSocketPath)
	}
}

// U-44: the snapshotter sidecar belongs to this driver's controller plugin and
// is applied whatever the cluster's own snapshot-controller is doing.
func TestSnapshotterSidecarIsAppliedRegardlessOfTheToggle(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.EnableVolumeSnapshots = ptr.To(false)

	if containerNamed(controllerStatefulSet(d, testImage).Spec.Template.Spec.Containers, "csi-snapshotter") == nil {
		t.Error("the snapshotter sidecar was dropped, which is the cluster's controller's toggle and not its own")
	}
}

// U-33 and U-34: the driver name reaches the kubelet registration path and the
// hostPath the node plugin mounts, which is what design §9 Q3 says the chart
// writes literally.
func TestDriverNameReachesTheKubeletPaths(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.DriverName = altDriverName
	ds := nodeDaemonSet(d, testImage)

	registrar := containerNamed(ds.Spec.Template.Spec.Containers, "csi-registrar")
	got, ok := argValue(registrar, "--kubelet-registration-path")
	if !ok || got != "/var/lib/kubelet/plugins/"+altDriverName+"/csi.sock" {
		t.Errorf("--kubelet-registration-path = %q, want the path for %q", got, altDriverName)
	}

	var socketDirPath string
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.Name == "socket-dir" {
			socketDirPath = v.HostPath.Path
		}
	}
	if socketDirPath != "/var/lib/kubelet/plugins/"+altDriverName {
		t.Errorf("socket-dir hostPath = %q, want the directory for %q", socketDirPath, altDriverName)
	}
}

// U-36: a non-default driver name leaves the default nowhere in the deployment.
func TestNoObjectCarriesTheDefaultAlongsideAnOverride(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.DriverName = altDriverName

	ds := nodeDaemonSet(d, testImage)
	for _, c := range ds.Spec.Template.Spec.Containers {
		for _, a := range c.Args {
			if strings.Contains(a, DefaultDriverName) {
				t.Errorf("%s carries the default driver name in %q", c.Name, a)
			}
		}
	}
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.HostPath != nil && strings.Contains(v.HostPath.Path, DefaultDriverName) {
			t.Errorf("volume %s carries the default driver name in %q", v.Name, v.HostPath.Path)
		}
	}
}

// U-07: the replica count reaches the controller plugin, which is the only one
// that has one. A DaemonSet has as many as there are workers.
func TestReplicasReachTheControllerOnly(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.ControllerReplicas = ptr.To(int32(3))

	sts := controllerStatefulSet(d, testImage)
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 3 {
		t.Errorf("replicas = %v, want 3", sts.Spec.Replicas)
	}
}

// U-08: placement is per plugin, and the two do not leak into each other. The
// chart offers both, and swapping them would move the node plugin off the
// workers whose volumes it attaches.
func TestPlacementReachesItsOwnPluginOnly(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.NodeSelector = map[string]string{"storage": "yes"}
	d.Spec.Tolerations = []corev1.Toleration{{Key: "node", Operator: corev1.TolerationOpExists}}
	d.Spec.ControllerNodeSelector = map[string]string{"role": "control"}
	d.Spec.ControllerTolerations = []corev1.Toleration{{Key: "control", Operator: corev1.TolerationOpExists}}

	ds := nodeDaemonSet(d, testImage).Spec.Template.Spec
	sts := controllerStatefulSet(d, testImage).Spec.Template.Spec

	if ds.NodeSelector["storage"] != "yes" || len(ds.NodeSelector) != 1 {
		t.Errorf("node plugin nodeSelector = %v, want only the node pair", ds.NodeSelector)
	}
	if sts.NodeSelector["role"] != "control" || len(sts.NodeSelector) != 1 {
		t.Errorf("controller nodeSelector = %v, want only the controller pair", sts.NodeSelector)
	}
	if len(ds.Tolerations) != 1 || ds.Tolerations[0].Key != "node" {
		t.Errorf("node plugin tolerations = %v, want only the node one", ds.Tolerations)
	}
	if len(sts.Tolerations) != 1 || sts.Tolerations[0].Key != "control" {
		t.Errorf("controller tolerations = %v, want only the controller one", sts.Tolerations)
	}
}

// U-09: the resource blocks reach their own plugin and not the other.
func TestResourcesReachTheirOwnPlugin(t *testing.T) {
	nodeReq := corev1.ResourceRequirements{Limits: corev1.ResourceList{"cpu": resource.MustParse("2")}}
	ctrlReq := corev1.ResourceRequirements{Limits: corev1.ResourceList{"cpu": resource.MustParse("1")}}

	d := testDriver("simplyblock")
	d.Spec.NodeResources = nodeReq
	d.Spec.ControllerResources = ctrlReq

	node := containerNamed(nodeDaemonSet(d, testImage).Spec.Template.Spec.Containers, "csi-node")
	if got := node.Resources.Limits.Cpu().String(); got != "2" {
		t.Errorf("node plugin cpu limit = %s, want 2", got)
	}
	controller := containerNamed(controllerStatefulSet(d, testImage).Spec.Template.Spec.Containers, "csi-controller")
	if got := controller.Resources.Limits.Cpu().String(); got != "1" {
		t.Errorf("controller plugin cpu limit = %s, want 1", got)
	}
	// The registrar is the node plugin's sidecar and takes no resource block:
	// the chart gives it none, and inventing one is a restart nobody asked for.
	registrar := containerNamed(nodeDaemonSet(d, testImage).Spec.Template.Spec.Containers, "csi-registrar")
	if len(registrar.Resources.Limits) != 0 {
		t.Errorf("csi-registrar has limits it did not have before: %v", registrar.Resources.Limits)
	}
}

// The node plugin runs privileged with SYS_ADMIN and SYS_MODULE, because it
// loads the NVMe-oF transports and mounts filesystems in the host namespace.
// Losing that is losing every attach in the cluster, so it is asserted rather
// than assumed.
func TestNodePluginKeepsThePrivilegeItNeeds(t *testing.T) {
	ds := nodeDaemonSet(testDriver("simplyblock"), testImage)
	spec := ds.Spec.Template.Spec

	node := containerNamed(spec.Containers, "csi-node")
	sc := node.SecurityContext
	if sc == nil || sc.Privileged == nil || !*sc.Privileged {
		t.Fatal("the node plugin is not privileged")
	}
	caps := map[corev1.Capability]bool{}
	for _, c := range sc.Capabilities.Add {
		caps[c] = true
	}
	if !caps["SYS_ADMIN"] || !caps["SYS_MODULE"] {
		t.Errorf("capabilities = %v, want SYS_ADMIN and SYS_MODULE", sc.Capabilities.Add)
	}
	if !spec.HostNetwork || spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Errorf("hostNetwork = %v with dnsPolicy %q, want host networking with the matching policy",
			spec.HostNetwork, spec.DNSPolicy)
	}
}

// The two plugins mount the configuration and credentials this deployment's own
// objects hold, not another deployment's.
func TestBothPluginsMountThisDeploymentsConfiguration(t *testing.T) {
	d := testDriver("tenant-b")
	n := names(d)

	for _, volumes := range [][]corev1.Volume{
		nodeDaemonSet(d, testImage).Spec.Template.Spec.Volumes,
		controllerStatefulSet(d, testImage).Spec.Template.Spec.Volumes,
	} {
		for _, v := range volumes {
			switch v.Name {
			case "csi-config":
				if v.ConfigMap.Name != n.configMap {
					t.Errorf("csi-config = %q, want %q", v.ConfigMap.Name, n.configMap)
				}
			case "csi-secret":
				if v.Secret.SecretName != n.secretV2 {
					t.Errorf("csi-secret = %q, want %q", v.Secret.SecretName, n.secretV2)
				}
			}
		}
	}
}

// The pull policy defaults to Always, because the default image is a moving
// tag. IfNotPresent against a tag that moved leaves half the workers on the old
// plugin, which is a skew inside one deployment that nothing reports.
func TestPullPolicyDefaultsAndOverrides(t *testing.T) {
	d := testDriver("simplyblock")
	if got := pullPolicy(d); got != corev1.PullAlways {
		t.Errorf("default pull policy = %q, want Always", got)
	}

	d.Spec.ImagePullPolicy = corev1.PullIfNotPresent
	if got := pullPolicy(d); got != corev1.PullIfNotPresent {
		t.Errorf("pull policy = %q, want the spec's IfNotPresent", got)
	}
	for _, c := range nodeDaemonSet(d, testImage).Spec.Template.Spec.Containers {
		if c.ImagePullPolicy != corev1.PullIfNotPresent {
			t.Errorf("%s pull policy = %q, want the spec's IfNotPresent", c.Name, c.ImagePullPolicy)
		}
	}
}
