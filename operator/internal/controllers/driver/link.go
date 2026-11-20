// csi-link on the two plugin pods: the arguments that dial the operator, the
// downward-API identity it verifies, and the token volume behind them.
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
// TLS follows spec.tls, the way the plugins' connection to the control plane
// does, and reuses what that already mounts: the CA bundle in the TLS volume
// verifies the operator, and under mutual TLS the plugin's client certificate
// in the same volume is presented to it. With TLS off the plugin dials
// plaintext, which is what the operator serves on such an install. The token
// authenticates the plugin either way, so plaintext publishes it to anything on
// the path.

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

// linkArgs are the arguments every plugin dials with: the operator's address,
// and the files in the TLS volume that secure the link when spec.tls asks for
// it.
func linkArgs(d *simplyblockv1alpha2.SimplyblockDriver) []string {
	name := linkServerName(d)
	args := []string{
		"--link",
		"--link-hub-address=" + name + ":" + strconv.Itoa(linkPort),
		"--link-server-name=" + name,
	}
	if tlsEnabled(d) {
		args = append(args, "--link-ca-file="+tlsMountPath+"/ca.crt")
	}
	if mutualTLSEnabled(d) {
		args = append(args,
			"--link-cert-file="+tlsMountPath+"/tls.crt",
			"--link-key-file="+tlsMountPath+"/tls.key")
	}
	return args
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

// linkVolumes is the projected token. The TLS material comes from the TLS
// volume, which tls.go puts on the pod.
func linkVolumes() []corev1.Volume {
	expiry := int64(linkTokenExpirySeconds)
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
	}
}

// linkVolumeMounts puts the token on a plugin container and on no sidecar: a
// sidecar holding it could open a link as this node.
func linkVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: linkTokenVolumeName, MountPath: linkTokenMountPath, ReadOnly: true},
	}
}
