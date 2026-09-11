// Whether the two plugins reach the control plane over TLS, and what that
// puts on their pods: the environment the driver's own TLS-dialing code
// reads, and the volume carrying the CA bundle and, for mutual TLS, the
// client certificate.
//
// This reproduces the chart's `simplyblock.tlsEnv`, `simplyblock.tlsVolumeMount`,
// and `simplyblock.clientTlsVolume`, which every deployment measured before
// spec.tls existed had rendered onto its csi-controller and csi-node
// containers unconditionally, gated only on the tls.enabled and
// tls.mutual_enabled Helm values spec.tls now replaces. Reproducing them
// byte-for-byte, FDB_TLS_* included even though neither plugin reads it, is
// what keeps a Created deployment's objects identical to an already-running
// one an operator adopts (workloads.go's header comment).
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §4.1.

package driver

import (
	corev1 "k8s.io/api/core/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// tlsVolumeName and tlsMountPath are fixed: both plugins mount the same
	// volume at the same path the driver's own TLS-dialing code reads from
	// (SB_TLS_CERTIFICATE_AUTHORITY and friends default under this path).
	tlsVolumeName = "tls"
	tlsMountPath  = "/etc/simplyblock/tls"

	// openshiftCABundleConfigMap is the cluster-wide CA bundle OpenShift's
	// service-ca operator writes. It is not derived from this object's name:
	// it is the cluster's bundle, shared by every component, the same
	// ConfigMap the chart's simplyblock.caVolume and simplyblock.clientTlsVolume
	// already name literally.
	openshiftCABundleConfigMap = "simplyblock-certificate-authority"
	// certManagerCABundleSecret is the CA-only bundle the chart's ClusterIssuer
	// issues into the release namespace for the non-mutual case. Also fixed,
	// for the same reason.
	certManagerCABundleSecret = "simplyblock-ca-bundle-tls"
)

// tlsEnabled and mutualTLSEnabled apply the CRD's defaults, mirroring
// driverName's reasoning: code reading spec.tls should not have to care
// whether admission had a chance to default it.
func tlsEnabled(d *simplyblockv1alpha2.SimplyblockDriver) bool {
	return d.Spec.TLS.EnableTLS != nil && *d.Spec.TLS.EnableTLS
}

func mutualTLSEnabled(d *simplyblockv1alpha2.SimplyblockDriver) bool {
	return tlsEnabled(d) && d.Spec.TLS.EnableMutualTLS != nil && *d.Spec.TLS.EnableMutualTLS
}

// tlsProvider is spec.tls.provider with the CRD's default applied.
func tlsProvider(d *simplyblockv1alpha2.SimplyblockDriver) simplyblockv1alpha2.DriverTLSProvider {
	if d.Spec.TLS.Provider != "" {
		return d.Spec.TLS.Provider
	}
	return simplyblockv1alpha2.DriverTLSProviderCertManager
}

// tlsConnectMode is SB_TLS_CONNECT's value for spec.tls, or "disabled" for a
// deployment with no SB_TLS_CONNECT at all — the same string
// runningTLSConnectMode reads back off a live DaemonSet, so the two can be
// compared directly in adoption.go.
func tlsConnectMode(d *simplyblockv1alpha2.SimplyblockDriver) string {
	switch {
	case !tlsEnabled(d):
		return "disabled"
	case mutualTLSEnabled(d):
		return "authenticated"
	default:
		return "anonymous"
	}
}

// tlsEnv is simplyblock.tlsEnv for spec.tls. FDB_TLS_* has no reader in
// either plugin; it is here because it was already on every running
// deployment and adoption must not have to strip it back off.
func tlsEnv(d *simplyblockv1alpha2.SimplyblockDriver) []corev1.EnvVar {
	if !tlsEnabled(d) {
		return nil
	}
	env := []corev1.EnvVar{
		{Name: "SB_TLS_SERVE", Value: "1"},
		{Name: "SB_TLS_PROVIDER", Value: string(tlsProvider(d))},
	}
	if mutualTLSEnabled(d) {
		env = append(env,
			corev1.EnvVar{Name: "SB_TLS_CLIENT_AUTH", Value: "required"},
			corev1.EnvVar{Name: "SB_TLS_CONNECT", Value: "authenticated"},
			corev1.EnvVar{Name: "FDB_TLS_CERTIFICATE_FILE", Value: tlsMountPath + "/tls.crt"},
			corev1.EnvVar{Name: "FDB_TLS_KEY_FILE", Value: tlsMountPath + "/tls.key"},
			corev1.EnvVar{Name: "FDB_TLS_CA_FILE", Value: tlsMountPath + "/ca.crt"},
		)
	} else {
		env = append(env, corev1.EnvVar{Name: "SB_TLS_CONNECT", Value: "anonymous"})
	}
	return env
}

// tlsVolume is simplyblock.clientTlsVolume (mutual) or simplyblock.caVolume
// (not), for the plugin whose client-certificate Secret is clientSecret. Nil
// when TLS is off, which is every deployment before this field existed.
func tlsVolume(d *simplyblockv1alpha2.SimplyblockDriver, clientSecret string) *corev1.Volume {
	if !tlsEnabled(d) {
		return nil
	}
	if !mutualTLSEnabled(d) {
		return caOnlyVolume(d)
	}

	if tlsProvider(d) == simplyblockv1alpha2.DriverTLSProviderOpenShift {
		return &corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: clientSecret},
						}},
						{ConfigMap: &corev1.ConfigMapProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: openshiftCABundleConfigMap},
							Items:                []corev1.KeyToPath{{Key: "service-ca.crt", Path: "ca.crt"}},
						}},
					},
				},
			},
		}
	}
	// cert-manager: the Certificate this chart's controlplane_certificates.yaml
	// issues under clientSecret already carries tls.crt, tls.key, and ca.crt
	// together, so a plain Secret volume is enough.
	v := secretVolume(tlsVolumeName, clientSecret)
	return &v
}

// caOnlyVolume is the CA bundle alone, for a deployment with TLS on but
// mutual TLS off: the plugin dials over an encrypted connection but presents
// no client identity.
func caOnlyVolume(d *simplyblockv1alpha2.SimplyblockDriver) *corev1.Volume {
	if tlsProvider(d) == simplyblockv1alpha2.DriverTLSProviderOpenShift {
		return &corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: openshiftCABundleConfigMap},
					Items:                []corev1.KeyToPath{{Key: "service-ca.crt", Path: "ca.crt"}},
				},
			},
		}
	}
	return &corev1.Volume{
		Name: tlsVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: certManagerCABundleSecret,
				Items:      []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
			},
		},
	}
}

// tlsVolumeMount is simplyblock.tlsVolumeMount: the same mount on both
// plugins, present only when TLS is on.
func tlsVolumeMount(d *simplyblockv1alpha2.SimplyblockDriver) []corev1.VolumeMount {
	if !tlsEnabled(d) {
		return nil
	}
	return []corev1.VolumeMount{{Name: tlsVolumeName, MountPath: tlsMountPath, ReadOnly: true}}
}
