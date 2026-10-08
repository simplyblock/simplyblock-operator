// The metadata server's pod as the guest contract and design-pnfs-mds-vm.md §5
// need it: what it runs, what it may open on the node, how the guest is sized,
// and the state disk it keeps.

package driver

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const testClusterID = "0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90"

func withMDS(d *simplyblockv1alpha2.SimplyblockDriver) *simplyblockv1alpha2.SimplyblockDriver {
	d.Spec.PNFS.MDS = &simplyblockv1alpha2.DriverPNFSMDS{
		Image:        "quay.io/simplyblock-io/spdkcsi:pnfs-mds-v26.3.0",
		NodeSelector: map[string]string{"pool": "kvm"},
		Tolerations:  []corev1.Toleration{{Key: "kvm", Operator: corev1.TolerationOpExists}},
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}},
		StateSize: resource.MustParse("2Gi"),
	}
	return d
}

func mdsObjects(t *testing.T, d *simplyblockv1alpha2.SimplyblockDriver) (*corev1.ServiceAccount, *appsv1.StatefulSet) {
	t.Helper()
	sa, sts, err := MDSObjects(d, testClusterID, "simplyblock-state")
	if err != nil {
		t.Fatalf("MDSObjects: %v", err)
	}
	return sa, sts
}

func runnerContainer(t *testing.T, sts *appsv1.StatefulSet) *corev1.Container {
	t.Helper()
	containers := sts.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("want one container, got %d", len(containers))
	}
	return &containers[0]
}

func TestMDSImageDefaultsToTheOperatorsBuildInTheDriversRepository(t *testing.T) {
	t.Setenv(OperatorImageEnv, "quay.io/simplyblock-io/simplyblock-operator:v26.2.6")
	d := withMDS(testDriver("simplyblock"))
	d.Spec.PNFS.MDS.Image = ""

	got, err := mdsImage(d)
	if want := "quay.io/simplyblock-io/spdkcsi:pnfs-mds-v26.2.6"; err != nil || got != want {
		t.Errorf("mdsImage = %q, %v, want %q", got, err, want)
	}
	if !imagePattern.MatchString(got) {
		t.Errorf("the default %q does not satisfy the pattern the CRD enforces", got)
	}

	d.Spec.PNFS.MDS.Image = "docker.io/simplyblock/spdkcsi:pnfs-mds-pinned"
	if got, err := mdsImage(d); err != nil || got != d.Spec.PNFS.MDS.Image {
		t.Errorf("mdsImage = %q, %v, want the spec's", got, err)
	}
}

func TestMDSImageWithoutAnAnswerIsAnError(t *testing.T) {
	t.Setenv(OperatorImageEnv, "")
	d := withMDS(testDriver("simplyblock"))
	d.Spec.PNFS.MDS.Image = ""
	if got, err := mdsImage(d); err == nil {
		t.Errorf("mdsImage = %q, want an error naming what to set", got)
	}
}

// A StatefulSet's name is limited to 52 characters: its pods carry a
// controller-revision-hash label of the name plus a suffix, and a label is 63.
// A cluster UUID alone is 36, so the name carries a prefix of it and the label
// the whole.
func TestMDSStatefulSetNameFitsAndIdentifiesTheCluster(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	_, sts := mdsObjects(t, d)

	name := MDSStatefulSetName(d, testClusterID)
	if sts.Name != name || sts.Namespace != d.Namespace {
		t.Errorf("StatefulSet %s/%s, want %s/%s", sts.Namespace, sts.Name, d.Namespace, name)
	}
	if len(name) > 52 || len(validation.IsDNS1123Label(name)) > 0 {
		t.Errorf("name %q is not a valid StatefulSet name of at most 52 characters", name)
	}
	if !strings.Contains(name, testClusterID[:8]) {
		t.Errorf("name %q does not carry the cluster ID's prefix", name)
	}
	if got := sts.Spec.Template.Labels[MDSClusterLabel]; got != testClusterID {
		t.Errorf("pod label %s = %q, want the full cluster ID", MDSClusterLabel, got)
	}
	if other := MDSStatefulSetName(d, "7c3e9a10-0000-4000-8000-000000000000"); other == name {
		t.Errorf("two storage clusters share the StatefulSet %q", name)
	}
	if got := MDSPodName(d, testClusterID); got != name+"-0" {
		t.Errorf("MDSPodName = %q, want %q", got, name+"-0")
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want exactly one: two guests must never mount one filesystem", sts.Spec.Replicas)
	}
}

// The hub admits the MDS kind for this ServiceAccount only.
func TestMDSPodRunsUnderItsOwnServiceAccount(t *testing.T) {
	sa, sts := mdsObjects(t, withMDS(testDriver("simplyblock")))
	if sa.Name != "simplyblock-csi-mds-sa" || sa.Namespace != "simplyblock" {
		t.Errorf("ServiceAccount %s/%s, want simplyblock/simplyblock-csi-mds-sa", sa.Namespace, sa.Name)
	}
	if got := sts.Spec.Template.Spec.ServiceAccountName; got != sa.Name {
		t.Errorf("pod ServiceAccount = %q, want %q", got, sa.Name)
	}
}

func TestMDSPodRunsTheRunnerPrivilegedOnAKVMNode(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	_, sts := mdsObjects(t, d)
	pod := sts.Spec.Template.Spec
	c := runnerContainer(t, sts)

	if c.Image != d.Spec.PNFS.MDS.Image {
		t.Errorf("image = %q, want %q", c.Image, d.Spec.PNFS.MDS.Image)
	}
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Error("runner is not privileged; it cannot open /dev/kvm and /dev/net/tun without a device plugin")
	}
	if pod.NodeSelector[KVMCapableLabel] != KVMCapableValue || pod.NodeSelector["pool"] != "kvm" {
		t.Errorf("nodeSelector = %v, want the kvm-capable label merged with the spec's", pod.NodeSelector)
	}
	if !slices.Equal(pod.Tolerations, d.Spec.PNFS.MDS.Tolerations) {
		t.Errorf("tolerations = %v, want the spec's", pod.Tolerations)
	}
	if pod.HostNetwork || pod.HostPID || pod.HostIPC {
		t.Error("the pod shares a host namespace; the guest reaches the node only through the runner")
	}
}

// Privilege is unavoidable, host state is not: the two device nodes are the
// only host paths the pod mounts.
func TestMDSPodMountsOnlyTheTwoHostDevices(t *testing.T) {
	_, sts := mdsObjects(t, withMDS(testDriver("simplyblock")))
	var hostPaths []string
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.HostPath != nil {
			hostPaths = append(hostPaths, v.HostPath.Path)
			if v.HostPath.Type == nil || *v.HostPath.Type != corev1.HostPathCharDev {
				t.Errorf("host path %s is not required to be a character device", v.HostPath.Path)
			}
		}
	}
	slices.Sort(hostPaths)
	if want := []string{"/dev/kvm", "/dev/net/tun"}; !slices.Equal(hostPaths, want) {
		t.Errorf("host paths = %v, want only %v", hostPaths, want)
	}
}

// One setting sizes both: the runner reads the pod's own limits in the units
// it parses, and the guest's identity is the stable pod name.
func TestMDSGuestIsSizedAndNamedFromThePod(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	_, sts := mdsObjects(t, d)
	c := runnerContainer(t, sts)

	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	cpu := env["MDS_CPU_LIMIT_MILLI"].ValueFrom
	if cpu == nil || cpu.ResourceFieldRef == nil || cpu.ResourceFieldRef.Resource != "limits.cpu" ||
		!cpu.ResourceFieldRef.Divisor.Equal(resource.MustParse("1m")) {
		t.Errorf("MDS_CPU_LIMIT_MILLI = %+v, want limits.cpu in millicores", cpu)
	}
	memory := env["MDS_MEMORY_LIMIT_MIB"].ValueFrom
	if memory == nil || memory.ResourceFieldRef == nil || memory.ResourceFieldRef.Resource != "limits.memory" ||
		!memory.ResourceFieldRef.Divisor.Equal(resource.MustParse("1Mi")) {
		t.Errorf("MDS_MEMORY_LIMIT_MIB = %+v, want limits.memory in MiB", memory)
	}
	if name := env["POD_NAME"].ValueFrom; name == nil || name.FieldRef == nil || name.FieldRef.FieldPath != "metadata.name" {
		t.Errorf("POD_NAME = %+v, want the pod's own name", name)
	}
	if !c.Resources.Limits.Cpu().Equal(resource.MustParse("2")) ||
		!c.Resources.Limits.Memory().Equal(resource.MustParse("2Gi")) {
		t.Errorf("resources = %+v, want the spec's", c.Resources)
	}
}

// The state disk is a raw block device the guest formats itself, at the path
// the runner attaches it from, in the class the caller chose for it.
func TestMDSStateDiskIsABlockClaimAtTheRunnersPath(t *testing.T) {
	const class = "simplyblock-state" // what mdsObjects passes as the caller's choice
	d := withMDS(testDriver("simplyblock"))
	_, sts := mdsObjects(t, d)

	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("want one claim template, got %d", len(sts.Spec.VolumeClaimTemplates))
	}
	claim := sts.Spec.VolumeClaimTemplates[0]
	if claim.Spec.VolumeMode == nil || *claim.Spec.VolumeMode != corev1.PersistentVolumeBlock {
		t.Errorf("volumeMode = %v, want Block", claim.Spec.VolumeMode)
	}
	if !slices.Equal(claim.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) {
		t.Errorf("accessModes = %v, want ReadWriteOnce", claim.Spec.AccessModes)
	}
	if got := claim.Spec.Resources.Requests[corev1.ResourceStorage]; !got.Equal(resource.MustParse("2Gi")) {
		t.Errorf("size = %s, want 2Gi", got.String())
	}
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != class {
		t.Errorf("storageClassName = %v, want %q", claim.Spec.StorageClassName, class)
	}

	c := runnerContainer(t, sts)
	if len(c.VolumeDevices) != 1 || c.VolumeDevices[0].Name != claim.Name ||
		c.VolumeDevices[0].DevicePath != "/dev/mds-state" {
		t.Errorf("volumeDevices = %+v, want the claim at /dev/mds-state", c.VolumeDevices)
	}
}

// An unset size takes the CRD's default rather than a zero-sized claim, which
// the API server would refuse.
func TestMDSStateDiskDefaultsItsSize(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	d.Spec.PNFS.MDS.StateSize = resource.Quantity{}
	_, sts := mdsObjects(t, d)
	got := sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]
	if !got.Equal(resource.MustParse("1Gi")) {
		t.Errorf("size = %s, want 1Gi", got.String())
	}
}

// Ready only while the guest is healthy, and the termination grace leaves the
// runner its own shutdown grace to power the guest down in.
func TestMDSPodReadinessAndShutdownFollowTheGuest(t *testing.T) {
	_, sts := mdsObjects(t, withMDS(testDriver("simplyblock")))
	c := runnerContainer(t, sts)

	probe := c.ReadinessProbe
	if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Path != "/readyz" {
		t.Errorf("readiness probe = %+v, want GET /readyz", probe)
	}
	if !slices.ContainsFunc(c.Ports, func(p corev1.ContainerPort) bool {
		return p.ContainerPort == 2049 && p.Protocol == corev1.ProtocolTCP
	}) {
		t.Errorf("ports = %+v, want NFS on TCP 2049", c.Ports)
	}
	grace := sts.Spec.Template.Spec.TerminationGracePeriodSeconds
	if grace == nil || *grace <= 20 {
		t.Errorf("terminationGracePeriodSeconds = %v, want more than the runner's 20s shutdown grace", grace)
	}
}

func TestMDSObjectsNeedTheMDSSpec(t *testing.T) {
	if _, _, err := MDSObjects(testDriver("simplyblock"), testClusterID, "simplyblock-state"); err == nil {
		t.Error("MDSObjects rendered a metadata server for a driver without spec.pnfs.mds")
	}
}

// The runner dials the operator the way both plugins do, so the guest's
// export calls reach it over the same link.
func TestMDSRunnerDialsTheOperatorLikeThePlugins(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	_, sts := mdsObjects(t, d)
	c := runnerContainer(t, sts)
	for _, want := range linkArgs(d) {
		if !slices.Contains(c.Args, want) {
			t.Errorf("args %v lack %q", c.Args, want)
		}
	}
}

// Without limits the downward API reports the node's whole allocatable
// capacity, and the runner would size the guest to the entire node. A spec
// that states none gets a guest of a bounded size instead.
func TestMDSPodWithoutLimitsGetsDefaultOnes(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	d.Spec.PNFS.MDS.Resources = corev1.ResourceRequirements{}
	_, sts := mdsObjects(t, d)
	limits := runnerContainer(t, sts).Resources.Limits
	if !limits.Cpu().Equal(resource.MustParse("2")) || !limits.Memory().Equal(resource.MustParse("2Gi")) {
		t.Errorf("limits = %v, want 2 CPUs and 2Gi", limits)
	}
}

// A limit the spec states is kept, and only the missing one is defaulted.
func TestMDSPodKeepsTheLimitsItIsGiven(t *testing.T) {
	d := withMDS(testDriver("simplyblock"))
	d.Spec.PNFS.MDS.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("4Gi"),
	}}
	_, sts := mdsObjects(t, d)
	limits := runnerContainer(t, sts).Resources.Limits
	if !limits.Memory().Equal(resource.MustParse("4Gi")) || !limits.Cpu().Equal(resource.MustParse("2")) {
		t.Errorf("limits = %v, want the spec's 4Gi and a default 2 CPUs", limits)
	}
}
