// What an installed control plane needs in order to serve TLS.
//
// The control-plane image already speaks it, and has for as long as the chart
// installed the control plane itself: SB_TLS_SERVE turns the listener on,
// SB_TLS_PROVIDER says who signed the certificate, SB_TLS_CONNECT says what the
// control plane's own outbound calls do, SB_TLS_CLIENT_AUTH says what it demands
// of a caller, and the FDB_TLS_* trio is FoundationDB's peer TLS. None of it was
// reachable once the install moved off the chart, because the ControlPlane spec
// had no field to carry the decision, so the install was the plaintext one
// whatever a deployment asked for.
//
// The certificates themselves are not minted here. They are cert-manager
// Certificates and an OpenShift service-CA annotation, and the names below are
// the ones the chart has always issued them under, for the same reason names.go
// gives: a running deployment refers to them from places this operator does not
// control.

package controlplane

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// tlsVolumeName is the volume every control-plane pod mounts its serving
	// material from, and tlsMountPath is where the image looks for it.
	tlsVolumeName = "tls"
	tlsMountPath  = "/etc/simplyblock/tls"

	// ServingCertSecret holds the management API's serving certificate. It is
	// the secret the chart's Certificate issues into and the one a deployment
	// migrating off the chart already has.
	ServingCertSecret = "simplyblock-webappapi-tls"

	// openShiftCAConfigMap is where the OpenShift service CA publishes the
	// bundle its own certificates verify against. It arrives as a ConfigMap
	// under a key of its own, so the projection below renames it to the ca.crt
	// the image reads.
	openShiftCAConfigMap = "simplyblock-certificate-authority"
	openShiftCAKey       = "service-ca.crt"
)

// tlsEnv is the TLS half of a control-plane container's environment.
//
// SB_TLS_CONNECT is the pair's subtlety: it is what the control plane does when
// it is the client, and it follows the client-certificate decision rather than
// the serving one. A deployment that serves TLS without mutual TLS connects
// anonymously, because there is no certificate for it to present.
func tlsEnv(local *simplyblockv1alpha2.LocalControlPlane) []corev1.EnvVar {
	if !local.ServesTLS() {
		return nil
	}

	env := []corev1.EnvVar{
		{Name: "SB_TLS_SERVE", Value: "1"},
		{Name: "SB_TLS_PROVIDER", Value: string(local.TLSProvider())},
	}

	if !local.RequiresClientCertificate() {
		return append(env, corev1.EnvVar{Name: "SB_TLS_CONNECT", Value: "anonymous"})
	}

	return append(env,
		corev1.EnvVar{Name: "SB_TLS_CLIENT_AUTH", Value: "required"},
		corev1.EnvVar{Name: "SB_TLS_CONNECT", Value: "authenticated"},
		corev1.EnvVar{Name: "FDB_TLS_CERTIFICATE_FILE", Value: tlsMountPath + "/tls.crt"},
		corev1.EnvVar{Name: "FDB_TLS_KEY_FILE", Value: tlsMountPath + "/tls.key"},
		corev1.EnvVar{Name: "FDB_TLS_CA_FILE", Value: tlsMountPath + "/ca.crt"},
	)
}

// tlsMount is the mount that goes with it, and nothing when the deployment is
// plaintext.
func tlsMount(local *simplyblockv1alpha2.LocalControlPlane) []corev1.VolumeMount {
	if !local.ServesTLS() {
		return nil
	}
	return []corev1.VolumeMount{{
		Name:      tlsVolumeName,
		MountPath: tlsMountPath,
		ReadOnly:  true,
	}}
}

// tlsVolume is the material itself, which differs by issuer.
//
// cert-manager writes the certificate, its key, and the issuing CA into one
// Secret, so the Secret is the volume. The OpenShift service CA writes only the
// certificate and key there and publishes its bundle as a ConfigMap, so the two
// are projected together and the bundle's key is renamed to the ca.crt the image
// reads from either provider.
func tlsVolume(local *simplyblockv1alpha2.LocalControlPlane, secret string) []corev1.Volume {
	if !local.ServesTLS() {
		return nil
	}

	if local.TLSProvider() == simplyblockv1alpha2.ControlPlaneTLSOpenShift {
		return []corev1.Volume{{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: secret},
						}},
						{ConfigMap: &corev1.ConfigMapProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: openShiftCAConfigMap},
							Items: []corev1.KeyToPath{{
								Key: openShiftCAKey, Path: "ca.crt",
							}},
						}},
					},
				},
			},
		}}
	}

	return []corev1.Volume{{
		Name: tlsVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: secret},
		},
	}}
}

// servingCertificateObjects is what the install has to create so that the
// certificate its pods mount exists.
//
// It is the operator's job because the Service is. On the chart this lived next
// to the Service it certifies, and the install that replaced the chart took the
// Service without taking the certificate, so every pod mounted a Secret nothing
// produced.
//
// The two issuers do it differently and neither is a choice made here.
// cert-manager takes a Certificate naming the Service's DNS names, and the
// OpenShift service CA takes an annotation on the Service itself, which is why
// this returns objects for one and annotations for the other.
func servingCertificateObjects(cp *simplyblockv1alpha2.ControlPlane) []client.Object {
	local := cp.Spec.Source.Local
	if !local.ServesTLS() || local.TLSProvider() != simplyblockv1alpha2.ControlPlaneTLSCertManager {
		return nil
	}
	return []client.Object{
		utils.BuildServiceServingCertificate(cp.Namespace, ComponentWebAPI, ServingCertSecret),
	}
}

// servingCertAnnotations is the OpenShift half: the service CA signs from an
// annotation on the Service and writes the result into the Secret it names.
func servingCertAnnotations(cp *simplyblockv1alpha2.ControlPlane) map[string]string {
	local := cp.Spec.Source.Local
	return utils.ServingCertServiceAnnotations(
		local.ServesTLS(), string(local.TLSProvider()), ServingCertSecret)
}

// FoundationDB's peer TLS. The database's own connections are a separate
// listener from the management API's, with its own certificate, its own mount
// path, and a switch on the FoundationDBCluster rather than an environment
// variable: FDB_TLS_* supplies the material and mainContainer.enableTls is what
// makes the processes listen for it.
const (
	fdbTLSVolumeName = "tls-fdb"
	fdbTLSMountPath  = "/var/fdb/tls"

	// FDBPeerCertSecret is the certificate the database's processes present to
	// each other. It carries both usages, because every process is a server to
	// its peers and a client of them.
	FDBPeerCertSecret = "simplyblock-foundationdb-tls"

	// fdbPeerCertificateName is the Certificate that issues it, under the name
	// the chart used. Keeping the name is what makes this a handover rather than
	// a second issuer for one Secret: two Certificates naming one secretName are
	// two controllers writing to one place, and the narrower of them wins
	// whenever it happens to write last.
	fdbPeerCertificateName = "simplyblock-foundationdb"
)

// fdbPeerTLS reports whether the database's own connections are encrypted.
//
// It follows the client-certificate decision rather than the serving one. Peer
// TLS between database processes has no anonymous mode to fall back to: every
// process authenticates to every other or none of them do.
func fdbPeerTLS(cp *simplyblockv1alpha2.ControlPlane) bool {
	return cp.Spec.Source.Local.RequiresClientCertificate()
}

// fdbPeerEnv is the material FoundationDB reads, as the unstructured shape the
// FoundationDBCluster's pod templates take.
func fdbPeerEnv() []any {
	return []any{
		map[string]any{"name": "FDB_TLS_CERTIFICATE_FILE", "value": fdbTLSMountPath + "/tls.crt"},
		map[string]any{"name": "FDB_TLS_KEY_FILE", "value": fdbTLSMountPath + "/tls.key"},
		map[string]any{"name": "FDB_TLS_CA_FILE", "value": fdbTLSMountPath + "/ca.crt"},
	}
}

// fdbPeerVolume is the Secret the processes read it from.
func fdbPeerVolume() []any {
	return []any{map[string]any{
		"name":   fdbTLSVolumeName,
		"secret": map[string]any{"secretName": FDBPeerCertSecret},
	}}
}

// fdbPeerMount is where each process finds it.
func fdbPeerMount() []any {
	return []any{map[string]any{
		"name": fdbTLSVolumeName, "mountPath": fdbTLSMountPath, "readOnly": true,
	}}
}

// fdbOperatorPeerEnv is the same material for the FoundationDB operator itself,
// which reconciles the cluster and has to reach it the way its processes do.
func fdbOperatorPeerEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "FDB_TLS_CERTIFICATE_FILE", Value: fdbTLSMountPath + "/tls.crt"},
		{Name: "FDB_TLS_KEY_FILE", Value: fdbTLSMountPath + "/tls.key"},
		{Name: "FDB_TLS_CA_FILE", Value: fdbTLSMountPath + "/ca.crt"},
	}
}

// fdbOperatorPeerVolume and fdbOperatorPeerMount are the typed halves of the
// same, for the operator's own Deployment.
func fdbOperatorPeerVolume() []corev1.Volume {
	return []corev1.Volume{{
		Name: fdbTLSVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: FDBPeerCertSecret},
		},
	}}
}

func fdbOperatorPeerMount() []corev1.VolumeMount {
	return []corev1.VolumeMount{{
		Name: fdbTLSVolumeName, MountPath: fdbTLSMountPath, ReadOnly: true,
	}}
}

// fdbPeerCertificate issues the material, and carries both usages.
//
// Every database process is a server to its peers and a client of them, so a
// certificate with only server auth fails the half of the handshake where it is
// the client. That is why this is its own builder rather than the serving one
// above: BuildServiceServingCertificate names a server and nothing else.
//
// Nothing is issued under the OpenShift service CA, which signs from an
// annotation on a Service, and these processes have none.
func fdbPeerCertificate(cp *simplyblockv1alpha2.ControlPlane) []client.Object {
	if !fdbPeerTLS(cp) ||
		cp.Spec.Source.Local.TLSProvider() != simplyblockv1alpha2.ControlPlaneTLSCertManager {
		return nil
	}

	names := []any{
		ComponentFDBCluster,
		fmt.Sprintf("%s.%s", ComponentFDBCluster, cp.Namespace),
		fmt.Sprintf("%s.%s.svc", ComponentFDBCluster, cp.Namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", ComponentFDBCluster, cp.Namespace),
		fmt.Sprintf("*.%s.%s.svc.cluster.local", ComponentFDBCluster, cp.Namespace),
	}

	certificate := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":      fdbPeerCertificateName,
			"namespace": cp.Namespace,
		},
		"spec": map[string]any{
			"commonName": fdbPeerCertificateName,
			"secretName": FDBPeerCertSecret,
			"issuerRef": map[string]any{
				"kind": "ClusterIssuer",
				"name": utils.CertManagerClusterIssuerName,
			},
			"usages": []any{
				"digital signature", "key encipherment", "server auth", "client auth",
			},
			"dnsNames": names,
		},
	}}
	return []client.Object{certificate}
}
