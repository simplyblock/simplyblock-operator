// csi-link on the two plugin pods: the arguments that dial the operator, the
// downward-API identity it verifies, and the two volumes behind them.
//
// The direction reads backward. The plugin dials the operator and holds the
// connection open; the operator calls back down it. Nothing listens on a
// worker, which is the whole reason the link is shaped this way: no port on
// each host, no address to discover, and no NetworkPolicy beyond a pod reaching
// a Service.
//
// It is always on and has no spec surface. The operator serves the endpoint
// unconditionally and both plugins dial it, so there is nothing to configure
// and nothing that can disagree.
//
// TLS is not required, and is decided by whether the material is there. The CA
// ConfigMap is mounted optional: present, the plugin verifies the operator;
// absent, it dials plaintext, which is what the operator serves when no
// certificate was provisioned for it either. The token authenticates the plugin
// either way, so plaintext publishes it to anything on the path.

package driver

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// The mount path is the driver's own --link-token-file default, so moving
	// it would mean passing the flag too.
	linkTokenVolumeName = "csi-link-token"
	linkTokenMountPath  = "/var/run/secrets/simplyblock.io/link"

	// The bundle signing the operator's certificate, when there is one.
	linkCAVolumeName = "csi-link-ca"
	linkCAMountPath  = "/etc/simplyblock/csi-link-ca"
	linkCAConfigMap  = "simplyblock-csi-link-ca"
	linkCAKey        = "ca.crt"

	// How long a projected token lives. The kubelet rotates it and the agent
	// re-reads it, so this bounds replay, not the link.
	linkTokenExpirySeconds = 3600

	// The Service the operator is fronted by, the port it listens on, and the
	// audience its tokens are bound to. One string for the name and the
	// audience: a token is bound to the operator the plugin dials.
	linkServiceName = "simplyblock-csi-link"
	linkPort        = 9500
	linkAudience    = linkServiceName
)

// linkServerName is what a plugin verifies against the operator's certificate
// when there is one: the Service's qualified name, no port, because a SAN is a
// name.
func linkServerName(d *simplyblockv1alpha2.SimplyblockDriver) string {
	return linkServiceName + "." + d.Namespace + ".svc"
}

// linkArgs are the four arguments every plugin dials with.
func linkArgs(d *simplyblockv1alpha2.SimplyblockDriver) []string {
	name := linkServerName(d)
	return []string{
		"--link",
		"--link-hub-address=" + name + ":" + strconv.Itoa(linkPort),
		"--link-server-name=" + name,
		"--link-ca-file=" + linkCAMountPath + "/" + linkCAKey,
	}
}

// linkEnv is the identity the operator checks against the pod's token. Both
// halves come from the downward API, because a peer is not trusted to name
// itself.
func linkEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		fieldRefEnv("POD_UID", "metadata.uid"),
		fieldRefEnv("POD_NAME", "metadata.name"),
	}
}

// linkVolumes are the projected token and the CA bundle. The ConfigMap is
// optional, which is what makes TLS optional: no ConfigMap, no file, and the
// plugin dials plaintext.
func linkVolumes() []corev1.Volume {
	expiry := int64(linkTokenExpirySeconds)
	optional := true
	return []corev1.Volume{
		{
			Name: linkTokenVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Path:              "token",
							Audience:          linkAudience,
							ExpirationSeconds: &expiry,
						},
					}},
				},
			},
		},
		{
			Name: linkCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: linkCAConfigMap},
					Optional:             &optional,
				},
			},
		},
	}
}

// linkVolumeMounts puts both on a plugin container and on no sidecar: a sidecar
// holding the token could open a link as this node.
func linkVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: linkTokenVolumeName, MountPath: linkTokenMountPath, ReadOnly: true},
		{Name: linkCAVolumeName, MountPath: linkCAMountPath, ReadOnly: true},
	}
}
