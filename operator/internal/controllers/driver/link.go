// Whether the two plugins hold a link to the operator, and what that puts on
// their pods: the arguments that dial it, the downward-API identity the
// operator verifies, and the two volumes that authenticate it.
//
// This reproduces what the chart rendered behind csiLink.enabled, which is why
// the endpoint is assembled from a Service name and a port rather than taken as
// an address: the certificate the plugins verify carries a name, and a name
// with a port appended matches no SAN.
//
// The direction is worth stating because the arguments read backward. The node
// dials the operator and holds the connection open, and the operator issues
// calls back down it -- storage reads always, and pNFS export assembly when
// spec.pnfs is on. Nothing listens on a worker.
//
// Specified by operator/docs/designs/design-pnfs-rwx.md §14.1, which is what
// made the csi-link spec surface that used to be a TODO in workloads.go
// necessary rather than optional.

package driver

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// linkTokenVolumeName and linkTokenMountPath are fixed: the mount path is
	// the driver's own --link-token-file default, and a volume mounted
	// somewhere else would need the flag passed too, for no gain.
	linkTokenVolumeName = "csi-link-token"
	linkTokenMountPath  = "/var/run/secrets/simplyblock.io/link"

	// linkCAVolumeName and linkCAMountPath carry the bundle signing the
	// operator's serving certificate. The key within it is spec.link.caKey,
	// because a ConfigMap an administrator already has may spell it either way.
	linkCAVolumeName = "csi-link-ca"
	linkCAMountPath  = "/etc/simplyblock/csi-link-ca"

	// linkTokenExpirySeconds is how long a projected token lives. The kubelet
	// rotates it and the agent re-reads the file on every attempt, so this is a
	// bound on replay rather than on the link's lifetime.
	linkTokenExpirySeconds = 3600
)

// linkEnabled applies the CRD's default, mirroring tlsEnabled: code reading
// spec.link should not have to care whether admission had a chance to default
// it.
func linkEnabled(d *simplyblockv1alpha2.SimplyblockDriver) bool {
	return d.Spec.Link.EnableLink != nil && *d.Spec.Link.EnableLink
}

// linkServiceName, linkPort, linkCAConfigMap, linkCAKey, and linkAudience apply
// the CRD's defaults for the same reason.
func linkServiceName(d *simplyblockv1alpha2.SimplyblockDriver) string {
	if d.Spec.Link.ServiceName != "" {
		return d.Spec.Link.ServiceName
	}
	return "simplyblock-csi-link"
}

func linkPort(d *simplyblockv1alpha2.SimplyblockDriver) int32 {
	if d.Spec.Link.Port != nil {
		return *d.Spec.Link.Port
	}
	return 9500
}

func linkCAConfigMap(d *simplyblockv1alpha2.SimplyblockDriver) string {
	if d.Spec.Link.CAConfigMap != "" {
		return d.Spec.Link.CAConfigMap
	}
	return "simplyblock-csi-link-ca"
}

func linkCAKey(d *simplyblockv1alpha2.SimplyblockDriver) string {
	if d.Spec.Link.CAKey != "" {
		return d.Spec.Link.CAKey
	}
	return "ca.crt"
}

func linkAudience(d *simplyblockv1alpha2.SimplyblockDriver) string {
	if d.Spec.Link.Audience != "" {
		return d.Spec.Link.Audience
	}
	return "simplyblock-csi-link"
}

// linkServerName is the name the plugins verify against the operator's
// certificate: the Service's fully qualified name in this object's own
// namespace, with no port, because a SAN is a name.
func linkServerName(d *simplyblockv1alpha2.SimplyblockDriver) string {
	return linkServiceName(d) + "." + d.Namespace + ".svc"
}

// linkArgs are the four arguments a plugin dials with, or none when the link is
// off.
func linkArgs(d *simplyblockv1alpha2.SimplyblockDriver) []string {
	if !linkEnabled(d) {
		return nil
	}
	name := linkServerName(d)
	return []string{
		"--link",
		"--link-hub-address=" + name + ":" + strconv.Itoa(int(linkPort(d))),
		"--link-server-name=" + name,
		"--link-ca-file=" + linkCAMountPath + "/" + linkCAKey(d),
	}
}

// linkEnv is the identity the operator verifies against the pod's token. It
// refuses a link that disagrees, so both halves come from the downward API
// rather than from anything this controller writes.
func linkEnv(d *simplyblockv1alpha2.SimplyblockDriver) []corev1.EnvVar {
	if !linkEnabled(d) {
		return nil
	}
	return []corev1.EnvVar{
		fieldRefEnv("POD_UID", "metadata.uid"),
		fieldRefEnv("POD_NAME", "metadata.name"),
	}
}

// linkVolumes are the token and the CA bundle.
func linkVolumes(d *simplyblockv1alpha2.SimplyblockDriver) []corev1.Volume {
	if !linkEnabled(d) {
		return nil
	}
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
							Audience:          linkAudience(d),
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
					LocalObjectReference: corev1.LocalObjectReference{Name: linkCAConfigMap(d)},
					Optional:             &optional,
				},
			},
		},
	}
}

// linkVolumeMounts puts both on a plugin container, read-only, and on no
// sidecar: a sidecar holding the token could open a link as this node.
func linkVolumeMounts(d *simplyblockv1alpha2.SimplyblockDriver) []corev1.VolumeMount {
	if !linkEnabled(d) {
		return nil
	}
	return []corev1.VolumeMount{
		{Name: linkTokenVolumeName, MountPath: linkTokenMountPath, ReadOnly: true},
		{Name: linkCAVolumeName, MountPath: linkCAMountPath, ReadOnly: true},
	}
}
