// What csi-link puts on the two plugins, and what it must not put anywhere
// else.
//
// The pairs are the point: a plugin carrying the arguments and not the token
// has a link that cannot authenticate, and one carrying the token and not the
// arguments has a token nothing presents.

package driver

import (
	"slices"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func volumeNamed(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}

func mountNamed(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}
	return nil
}

func hasArg(c *corev1.Container, arg string) bool { return slices.Contains(c.Args, arg) }

func envNamed(c *corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

// Both plugins dial, and both are addressed at the operator's Service in their
// own namespace. The node plugin serves storage on the link; the controller
// plugin links so the operator can see it.
func TestLinkReachesBothPlugins(t *testing.T) {
	d := testDriver("simplyblock")
	name := linkServiceName + ".simplyblock.svc"
	want := name + ":" + strconv.Itoa(linkPort)

	node := containerNamed(nodeDaemonSet(d, testImage).Spec.Template.Spec.Containers, "csi-node")
	controller := containerNamed(
		controllerStatefulSet(d, testImage).Spec.Template.Spec.Containers, "csi-controller")

	for _, c := range []*corev1.Container{node, controller} {
		if !hasArg(c, "--link") {
			t.Errorf("%s does not carry --link", c.Name)
		}
		if got, _ := argValue(c, "--link-hub-address"); got != want {
			t.Errorf("%s dials %q, want %q", c.Name, got, want)
		}
		// The name verified against the certificate has no port: a SAN is a
		// name, and one with a port appended matches nothing.
		if got, _ := argValue(c, "--link-server-name"); got != name {
			t.Errorf("%s verifies %q, want %q", c.Name, got, name)
		}
	}
}

// The operator verifies the pod's claimed identity against its token and
// refuses a link that disagrees, so both halves come from the downward API.
func TestLinkedPluginsCarryTheirDownwardIdentity(t *testing.T) {
	d := testDriver("simplyblock")
	node := containerNamed(nodeDaemonSet(d, testImage).Spec.Template.Spec.Containers, "csi-node")

	for _, name := range []string{"POD_UID", "POD_NAME"} {
		e := envNamed(node, name)
		if e == nil {
			t.Fatalf("csi-node does not carry %s", name)
		}
		if e.ValueFrom == nil || e.ValueFrom.FieldRef == nil {
			t.Errorf("%s is not a downward-API reference, so a pod could claim to be another", name)
		}
	}
}

// A token bound to no audience is one anything holding it can replay against
// the operator, which is the whole reason the link authenticates.
func TestLinkTokenIsAudienceBound(t *testing.T) {
	d := testDriver("simplyblock")
	v := volumeNamed(nodeDaemonSet(d, testImage).Spec.Template.Spec.Volumes, linkTokenVolumeName)
	if v == nil || v.Projected == nil || len(v.Projected.Sources) == 0 {
		t.Fatal("the node plugin carries no projected token")
	}
	token := v.Projected.Sources[0].ServiceAccountToken
	if token == nil || token.Audience != linkAudience {
		t.Errorf("token audience = %+v, want %q", token, linkAudience)
	}
}

// TLS is optional, and the optional mount is what makes it so: no ConfigMap,
// no file, and the agent dials plaintext to match an operator serving it.
func TestLinkCAVolumeIsOptional(t *testing.T) {
	d := testDriver("simplyblock")
	v := volumeNamed(nodeDaemonSet(d, testImage).Spec.Template.Spec.Volumes, linkCAVolumeName)
	if v == nil || v.ConfigMap == nil {
		t.Fatal("the node plugin carries no CA volume")
	}
	if v.ConfigMap.Optional == nil || !*v.ConfigMap.Optional {
		t.Error("the CA ConfigMap is required, so a cluster without one cannot start its plugins")
	}
	if v.ConfigMap.Name != linkCAConfigMap {
		t.Errorf("CA ConfigMap = %q, want %q", v.ConfigMap.Name, linkCAConfigMap)
	}
}

// Neither volume reaches a sidecar: one holding the token could open a link as
// this node.
func TestLinkVolumesReachOnlyThePluginContainers(t *testing.T) {
	d := testDriver("simplyblock")

	for _, pod := range []corev1.PodSpec{
		nodeDaemonSet(d, testImage).Spec.Template.Spec,
		controllerStatefulSet(d, testImage).Spec.Template.Spec,
	} {
		for _, c := range pod.Containers {
			plugin := c.Name == "csi-node" || c.Name == "csi-controller"
			for _, name := range []string{linkTokenVolumeName, linkCAVolumeName} {
				m := mountNamed(c.VolumeMounts, name)
				switch {
				case plugin && m == nil:
					t.Errorf("%s does not mount %s", c.Name, name)
				case plugin && !m.ReadOnly:
					t.Errorf("%s mounts %s writable", c.Name, name)
				case !plugin && m != nil:
					t.Errorf("sidecar %s mounts %s", c.Name, name)
				}
			}
		}
	}
}
