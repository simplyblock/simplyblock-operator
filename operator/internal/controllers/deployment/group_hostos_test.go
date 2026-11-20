// A group's own host OS: what it gives the nodes the group expands into.
//
// A cluster whose workers run more than one operating system, such as an EKS
// cluster with Amazon Linux and Ubuntu node groups, has no single host OS for
// the document. The document's spec.hostOS is the fleet's default, and a group
// states its own where it differs.

package deployment

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The node carries the operating system whole, release included, and resolves
// it itself, so a later decision on the family or the version has it.
func TestAGroupsHostOSReachesItsNodes(t *testing.T) {
	ubuntu := simplyblockv1alpha2.HostOSSpec{
		Distro:  simplyblockv1alpha2.DistroUbuntu,
		Family:  simplyblockv1alpha2.HostOSFamilyDebian,
		Version: "24.04",
	}
	config := aDocument(func(d *simplyblockv1alpha2.ClusterDeploymentConfig) {
		d.Spec.HostOS = &simplyblockv1alpha2.HostOSSpec{
			Distro: "amzn", Family: simplyblockv1alpha2.HostOSFamilyRedHat, Version: "2023",
		}
		d.Spec.NodeSets[0].Groups[0].HostOS = ubuntu.DeepCopy()
	})

	got := nodeOfGroup(t, config).Spec.Config.HostOS
	if got == nil || !equality.Semantic.DeepEqual(*got, ubuntu) {
		t.Errorf("the node states hostOS=%+v, want the group's %+v", got, ubuntu)
	}
}

// A group that states no host OS runs the document's, which the cluster already
// carries, so its nodes state nothing and the fleet's value decides.
func TestAGroupWithNoHostOSStatesNoneOnItsNodes(t *testing.T) {
	config := aDocument(func(d *simplyblockv1alpha2.ClusterDeploymentConfig) {
		d.Spec.HostOS = &simplyblockv1alpha2.HostOSSpec{Distro: simplyblockv1alpha2.DistroUbuntu}
	})

	if got := nodeOfGroup(t, config).Spec.Config.HostOS; got != nil {
		t.Errorf("the node states hostOS=%+v with no group host OS", *got)
	}
}

// A group's host OS is a statement about its workers' os-release, held to the
// same shape as the document's.
func TestAGroupsHostOSIsAdmittedAndHeldToItsShape(t *testing.T) {
	err := createDocument(t, "group-hostos-amzn", func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.NodeSets[0].Groups[0].HostOS = &simplyblockv1alpha2.HostOSSpec{Distro: "amzn", Version: "2"}
	})
	if err != nil {
		t.Fatalf("a group stating Amazon Linux 2 was refused: %v", err)
	}

	err = createDocument(t, "group-hostos-bad-version", func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.NodeSets[0].Groups[0].HostOS = &simplyblockv1alpha2.HostOSSpec{Distro: "amzn", Version: "Two Thousand"}
	})
	if err == nil {
		t.Error("a group whose version is not an os-release VERSION_ID was admitted")
	}
}
