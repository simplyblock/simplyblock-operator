// The site link on the CSI controller (design-stretched-sync-replication-ramen.md
// §7.3): on a member whose operator runs the site link, the controller plugin
// links to the site's fleet agent as well as to the operator, so the other
// site's operator can reach it.
//
// Only the controller plugin gets it. A node plugin does not speak across sites,
// and the agent refuses a node peer that tries, so giving one the token would
// only be a credential nothing should present.

package driver

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	siteLinkTokenVolumeName = "site-link-token"
	// The mount path is the driver's own --site-link-token-file default.
	siteLinkTokenMountPath = "/var/run/secrets/simplyblock.io/site-link"
	// siteLinkAudience is the audience the site's fleet agent verifies.
	siteLinkAudience = "simplyblock-site-link"
)

// SiteLinkConfig is the site link the CSI controller joins: the operator's own,
// taken from its flags.
type SiteLinkConfig struct {
	// Address is the site's fleet agent, host:port.
	Address string
	// AgentNamespace is where the agent publishes the CA it serves with.
	AgentNamespace string
}

// withSiteLink adds the site link to the controller plugin of sts. A nil cfg
// leaves sts as it is.
func withSiteLink(sts *appsv1.StatefulSet, cfg *SiteLinkConfig) *appsv1.StatefulSet {
	if cfg == nil {
		return sts
	}
	pod := &sts.Spec.Template.Spec
	expiry := int64(linkTokenExpirySeconds)
	pod.Volumes = append(pod.Volumes, corev1.Volume{
		Name: siteLinkTokenVolumeName,
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Path: "token", Audience: siteLinkAudience, ExpirationSeconds: &expiry,
			}}},
		}},
	})
	for i := range pod.Containers {
		c := &pod.Containers[i]
		if c.Name != controllerPluginName {
			continue
		}
		c.Args = append(c.Args,
			"--site-link",
			"--site-link-address="+cfg.Address,
			"--site-link-namespace="+cfg.AgentNamespace,
		)
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name: siteLinkTokenVolumeName, MountPath: siteLinkTokenMountPath, ReadOnly: true,
		})
	}
	return sts
}
