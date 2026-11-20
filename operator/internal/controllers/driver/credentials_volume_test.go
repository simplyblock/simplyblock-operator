// Tests that the plugins start before any storage cluster has written the
// credentials Secret, which node add needs because it reads host NICs through them.

package driver

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Regression: the plugins stayed in ContainerCreating until a StorageCluster
// wrote simplyblock-csi-secret-v2, so they could not answer the host checks the
// first cluster's node add depends on.
func TestTheCredentialsSecretDoesNotHoldThePluginsBack(t *testing.T) {
	d := testDriver("simplyblock")
	plugins := map[string][]corev1.Volume{
		"node":       nodeDaemonSet(d, testImage).Spec.Template.Spec.Volumes,
		"controller": controllerStatefulSet(d, testImage).Spec.Template.Spec.Volumes,
	}
	for plugin, volumes := range plugins {
		v := volumeNamed(volumes, "csi-secret")
		if v == nil || v.Secret == nil {
			t.Fatalf("%s plugin: no csi-secret Secret volume", plugin)
		}
		if v.Secret.Optional == nil || !*v.Secret.Optional {
			t.Errorf("%s plugin: csi-secret (%s) is required, so the pod cannot start before a cluster writes it",
				plugin, v.Secret.SecretName)
		}
	}
}
