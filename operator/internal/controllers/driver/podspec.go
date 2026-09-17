// The pod-spec shorthands the two workload builders are written in.
//
// They exist so that workloads.go reads as the list of volumes and mounts the
// chart renders rather than as nested struct literals, which is the only way the
// two can be compared by eye when one of them changes.

package driver

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/simplyblock/atlas/ptr"
)

// named addresses a container port by its name rather than its number, so a port
// that moves does not leave a probe pointing at the old one.
func named(port string) intstr.IntOrString {
	return intstr.FromString(port)
}

// fieldRefEnv reads a value off the pod itself through the downward API.
func fieldRefEnv(name, path string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: path},
		},
	}
}

func hostPathVolume(name, path string, kind *corev1.HostPathType) corev1.Volume {
	return corev1.Volume{
		Name:         name,
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path, Type: kind}},
	}
}

// configMapVolume mounts a ConfigMap. optional is set for the node-server
// configuration, which is empty in every deployment that does not run an xPU and
// whose absence must not stop the node plugin from starting.
func configMapVolume(name, configMap string, optional bool) corev1.Volume {
	source := &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: configMap},
	}
	if optional {
		source.Optional = ptr.To(true)
	}
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{ConfigMap: source}}
}

func secretVolume(name, secret string) corev1.Volume {
	return corev1.Volume{
		Name:         name,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret}},
	}
}

// concatEnv and concatMounts join a container's fixed entries to the optional
// groups each feature contributes. They exist because the alternative is
// nested appends, which is what the third feature turned unreadable: a group
// appended in the wrong place is a variable on the wrong plugin, and the nesting
// is exactly what hides it.
//
// Order is preserved, and an empty group contributes nothing, so a feature that
// is off leaves the container byte-identical to what it was before the feature
// existed. That is what adoption depends on.
func concatEnv(groups ...[]corev1.EnvVar) []corev1.EnvVar {
	var out []corev1.EnvVar
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func concatMounts(groups ...[]corev1.VolumeMount) []corev1.VolumeMount {
	var out []corev1.VolumeMount
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}
