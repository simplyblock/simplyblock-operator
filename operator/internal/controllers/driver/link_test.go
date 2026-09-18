// What spec.link puts on the two plugins, and what it must not put anywhere
// else.
//
// The pairs are the point. csi-link is how the operator reaches a node without
// anything listening there, so a plugin that carries the arguments and not the
// token has a link that cannot authenticate, and one that carries the token and
// not the arguments has a token nothing presents. Both halves are asserted
// together rather than separately.

package driver

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// linkedDriver is a deployment with the link on and everything else defaulted,
// which is what a chart install that set only the toggle produces.
func linkedDriver() *simplyblockv1alpha2.SimplyblockDriver {
	d := testDriver("simplyblock")
	d.Spec.Link.EnableLink = ptr.To(true)
	return d
}

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

// A deployment that does not ask for the link carries nothing of it. This is
// every deployment before the field existed, and it is what makes the field
// safe to add: an object written without it renders the pods it rendered
// before.
func TestLinkOffAddsNothing(t *testing.T) {
	d := testDriver("simplyblock")
	ds := nodeDaemonSet(d, testImage)
	sts := controllerStatefulSet(d, testImage)

	for _, pod := range []corev1.PodSpec{ds.Spec.Template.Spec, sts.Spec.Template.Spec} {
		for _, name := range []string{linkTokenVolumeName, linkCAVolumeName} {
			if volumeNamed(pod.Volumes, name) != nil {
				t.Errorf("a pod carries the %s volume with the link off", name)
			}
		}
		for _, c := range pod.Containers {
			if hasArg(&c, "--link") {
				t.Errorf("%s carries --link with the link off", c.Name)
			}
			if envNamed(&c, "POD_UID") != nil || envNamed(&c, "POD_NAME") != nil {
				t.Errorf("%s carries the downward-API identity with the link off", c.Name)
			}
		}
	}
}

// Both plugins dial, and both are addressed at the operator's Service in their
// own namespace. The node plugin is the one that serves storage and exports on
// the link; the controller plugin links so the operator can see it.
func TestLinkReachesBothPlugins(t *testing.T) {
	d := linkedDriver()
	const want = "simplyblock-csi-link.simplyblock.svc:9500"

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
		if got, _ := argValue(c, "--link-server-name"); got != "simplyblock-csi-link.simplyblock.svc" {
			t.Errorf("%s verifies %q", c.Name, got)
		}
		if got, _ := argValue(c, "--link-ca-file"); got != "/etc/simplyblock/csi-link-ca/ca.crt" {
			t.Errorf("%s reads the CA from %q", c.Name, got)
		}
	}
}

// The operator verifies the pod's claimed identity against its token and
// refuses a link that disagrees, so both halves come from the downward API
// rather than from anything this controller writes.
func TestLinkedPluginsCarryTheirDownwardIdentity(t *testing.T) {
	d := linkedDriver()

	node := containerNamed(nodeDaemonSet(d, testImage).Spec.Template.Spec.Containers, "csi-node")
	controller := containerNamed(
		controllerStatefulSet(d, testImage).Spec.Template.Spec.Containers, "csi-controller")

	for _, c := range []*corev1.Container{node, controller} {
		for field, path := range map[string]string{"POD_UID": "metadata.uid", "POD_NAME": "metadata.name"} {
			e := envNamed(c, field)
			if e == nil {
				t.Errorf("%s carries no %s", c.Name, field)
				continue
			}
			if e.ValueFrom == nil || e.ValueFrom.FieldRef == nil || e.ValueFrom.FieldRef.FieldPath != path {
				t.Errorf("%s reads %s from %+v, want the downward API %s", c.Name, field, e.ValueFrom, path)
			}
		}
	}
}

// The token is projected for the operator's audience specifically. A token
// bound to no particular audience is one anything holding it can replay against
// the operator, which is the whole reason the link authenticates at all.
func TestLinkTokenIsAudienceBound(t *testing.T) {
	d := linkedDriver()

	for _, pod := range []corev1.PodSpec{
		nodeDaemonSet(d, testImage).Spec.Template.Spec,
		controllerStatefulSet(d, testImage).Spec.Template.Spec,
	} {
		v := volumeNamed(pod.Volumes, linkTokenVolumeName)
		if v == nil || v.Projected == nil || len(v.Projected.Sources) != 1 {
			t.Fatalf("the link token volume is not a single projected source: %+v", v)
		}
		token := v.Projected.Sources[0].ServiceAccountToken
		if token == nil {
			t.Fatal("the link token volume projects something other than a token")
		}
		if token.Audience != "simplyblock-csi-link" {
			t.Errorf("token audience = %q, want the operator's", token.Audience)
		}
		if token.Path != "token" {
			t.Errorf("token path = %q, want it to match --link-token-file's default", token.Path)
		}
	}
}

// The CA ConfigMap is optional, because a publicly rooted certificate needs
// none and a missing one must not stop the plugin from starting: the agent
// falls back to the system roots.
func TestLinkCAVolumeIsOptional(t *testing.T) {
	d := linkedDriver()
	v := volumeNamed(nodeDaemonSet(d, testImage).Spec.Template.Spec.Volumes, linkCAVolumeName)
	if v == nil || v.ConfigMap == nil {
		t.Fatalf("the link CA volume is not a ConfigMap: %+v", v)
	}
	if v.ConfigMap.Name != "simplyblock-csi-link-ca" {
		t.Errorf("CA ConfigMap = %q", v.ConfigMap.Name)
	}
	if v.ConfigMap.Optional == nil || !*v.ConfigMap.Optional {
		t.Error("the link CA ConfigMap is required; a deployment without one cannot start")
	}
}

// Both volumes are mounted read-only on the plugin container, and on no
// sidecar: a sidecar holding the operator's token could open a link as this
// node.
func TestLinkVolumesReachOnlyThePluginContainers(t *testing.T) {
	d := linkedDriver()

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

// The endpoint is configurable, because the Service the operator is fronted by
// is a chart value rather than a constant.
func TestLinkEndpointFollowsTheSpec(t *testing.T) {
	d := linkedDriver()
	d.Spec.Link.ServiceName = "operator-link"
	d.Spec.Link.Port = ptr.To(int32(9600))
	d.Spec.Link.CAConfigMap = "operator-link-ca"
	d.Spec.Link.CAKey = "bundle.pem"
	d.Spec.Link.Audience = "operator-link-audience"

	pod := nodeDaemonSet(d, testImage).Spec.Template.Spec
	c := containerNamed(pod.Containers, "csi-node")
	if got, _ := argValue(c, "--link-hub-address"); got != "operator-link.simplyblock.svc:9600" {
		t.Errorf("hub address = %q", got)
	}
	if got, _ := argValue(c, "--link-ca-file"); got != "/etc/simplyblock/csi-link-ca/bundle.pem" {
		t.Errorf("CA file = %q", got)
	}
	if v := volumeNamed(pod.Volumes, linkCAVolumeName); v == nil || v.ConfigMap.Name != "operator-link-ca" {
		t.Errorf("CA ConfigMap = %+v", v)
	}
	v := volumeNamed(pod.Volumes, linkTokenVolumeName)
	if v == nil || v.Projected.Sources[0].ServiceAccountToken.Audience != "operator-link-audience" {
		t.Errorf("token audience = %+v", v)
	}
}
