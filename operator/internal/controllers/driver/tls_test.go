// What spec.tls turns into: SB_TLS_CONNECT's three values, the env and volume
// the chart used to render unconditionally, and that only the two plugin
// containers ever see any of it — never a sidecar, and never the registrar.

package driver

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// tlsConnectMode is the value adoption.go compares against a running
// deployment's SB_TLS_CONNECT, so the three states have to produce exactly
// the three strings the chart's simplyblock.tlsEnv did.
func TestTLSConnectMode(t *testing.T) {
	tests := []struct {
		name    string
		tls     simplyblockv1alpha2.DriverTLS
		wantEnv string
	}{
		{name: "unset", wantEnv: "disabled"},
		{name: "TLS, anonymous", tls: simplyblockv1alpha2.DriverTLS{EnableTLS: ptr.To(true)}, wantEnv: "anonymous"},
		{
			name: "TLS, mutual",
			tls: simplyblockv1alpha2.DriverTLS{
				EnableTLS: ptr.To(true), EnableMutualTLS: ptr.To(true),
			},
			wantEnv: "authenticated",
		},
		{
			name:    "mutual set but TLS off is ignored, same as the chart",
			tls:     simplyblockv1alpha2.DriverTLS{EnableMutualTLS: ptr.To(true)},
			wantEnv: "disabled",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := testDriver("simplyblock")
			d.Spec.TLS = tc.tls
			if got := tlsConnectMode(d); got != tc.wantEnv {
				t.Errorf("tlsConnectMode = %q, want %q", got, tc.wantEnv)
			}
		})
	}
}

// U-83-ish: TLS off is the shape of every deployment before spec.tls existed —
// no env, no volume, no mount at all.
func TestTLSOffAddsNothing(t *testing.T) {
	d := testDriver("simplyblock")

	if env := tlsEnv(d); env != nil {
		t.Errorf("tlsEnv = %v, want nil", env)
	}
	if v := tlsVolume(d, "irrelevant"); v != nil {
		t.Errorf("tlsVolume = %v, want nil", v)
	}
	if m := tlsVolumeMount(d); m != nil {
		t.Errorf("tlsVolumeMount = %v, want nil", m)
	}

	node := containerNamed(nodeDaemonSet(d, testImage).Spec.Template.Spec.Containers, "csi-node")
	if _, found := envValue(node.Env, "SB_TLS_SERVE"); found {
		t.Error("csi-node carries SB_TLS_SERVE with TLS off")
	}
	for _, v := range nodeDaemonSet(d, testImage).Spec.Template.Spec.Volumes {
		if v.Name == tlsVolumeName {
			t.Error("the node DaemonSet carries a tls volume with TLS off")
		}
	}
}

// The env the chart rendered unconditionally onto both plugin containers,
// reproduced exactly: SB_TLS_SERVE, SB_TLS_PROVIDER, and — only with mutual
// TLS — SB_TLS_CLIENT_AUTH and the FDB_TLS_* set that neither plugin reads
// but every measured deployment already carries.
func TestTLSEnvMatchesTheChart(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.TLS = simplyblockv1alpha2.DriverTLS{EnableTLS: ptr.To(true)}

	env := tlsEnv(d)
	want := map[string]string{
		"SB_TLS_SERVE":    "1",
		"SB_TLS_PROVIDER": "cert-manager",
		"SB_TLS_CONNECT":  "anonymous",
	}
	for name, wantValue := range want {
		if got, found := envValue(env, name); !found || got != wantValue {
			t.Errorf("%s = %q, found %v, want %q", name, got, found, wantValue)
		}
	}
	for _, absent := range []string{"SB_TLS_CLIENT_AUTH", "FDB_TLS_CERTIFICATE_FILE"} {
		if _, found := envValue(env, absent); found {
			t.Errorf("%s is set without EnableMutualTLS", absent)
		}
	}

	d.Spec.TLS.EnableMutualTLS = ptr.To(true)
	env = tlsEnv(d)
	want = map[string]string{
		"SB_TLS_CLIENT_AUTH":       "required",
		"SB_TLS_CONNECT":           "authenticated",
		"FDB_TLS_CERTIFICATE_FILE": tlsMountPath + "/tls.crt",
		"FDB_TLS_KEY_FILE":         tlsMountPath + "/tls.key",
		"FDB_TLS_CA_FILE":          tlsMountPath + "/ca.crt",
	}
	for name, wantValue := range want {
		if got, found := envValue(env, name); !found || got != wantValue {
			t.Errorf("mutual: %s = %q, found %v, want %q", name, got, found, wantValue)
		}
	}
}

// Only the two plugin containers get any of this — a sidecar or the
// registrar picking up TLS env or the TLS volume mount is a container this
// object never intended to reach the control plane at all.
func TestTLSReachesOnlyThePluginContainers(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.TLS = simplyblockv1alpha2.DriverTLS{EnableTLS: ptr.To(true)}

	registrar := containerNamed(nodeDaemonSet(d, testImage).Spec.Template.Spec.Containers, "csi-registrar")
	if _, found := envValue(registrar.Env, "SB_TLS_SERVE"); found {
		t.Error("csi-registrar carries SB_TLS_SERVE")
	}

	for _, name := range []string{"csi-provisioner", "csi-snapshotter", "csi-attacher", "csi-resizer", "csi-health-monitor"} {
		sidecar := containerNamed(controllerStatefulSet(d, testImage).Spec.Template.Spec.Containers, name)
		if sidecar == nil {
			t.Fatalf("%s is not applied", name)
		}
		if _, found := envValue(sidecar.Env, "SB_TLS_SERVE"); found {
			t.Errorf("%s carries SB_TLS_SERVE", name)
		}
		for _, m := range sidecar.VolumeMounts {
			if m.Name == tlsVolumeName {
				t.Errorf("%s mounts the tls volume", name)
			}
		}
	}
}

// The volume's shape: a plain CA bundle without mutual TLS, and each
// plugin's own client-certificate Secret with it, using the name names.go
// derives for that plugin and no other.
func TestTLSVolumeShape(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.TLS = simplyblockv1alpha2.DriverTLS{EnableTLS: ptr.To(true)}

	v := tlsVolume(d, names(d).nodeClientSecret)
	if v == nil || v.Secret == nil || v.Secret.SecretName != certManagerCABundleSecret {
		t.Fatalf("CA-only volume = %+v, want the cert-manager CA bundle secret", v)
	}

	d.Spec.TLS.EnableMutualTLS = ptr.To(true)
	node := tlsVolume(d, names(d).nodeClientSecret)
	controller := tlsVolume(d, names(d).controllerClientSecret)
	if node == nil || node.Secret == nil || node.Secret.SecretName != "simplyblock-csi-node-client-tls" {
		t.Errorf("node client volume = %+v, want simplyblock-csi-node-client-tls", node)
	}
	if controller == nil || controller.Secret == nil ||
		controller.Secret.SecretName != "simplyblock-csi-controller-client-tls" {
		t.Errorf("controller client volume = %+v, want simplyblock-csi-controller-client-tls", controller)
	}

	d.Spec.TLS.Provider = simplyblockv1alpha2.DriverTLSProviderOpenShift
	openshift := tlsVolume(d, names(d).nodeClientSecret)
	if openshift == nil || openshift.Projected == nil || len(openshift.Projected.Sources) != 2 {
		t.Errorf("openshift mutual volume = %+v, want a projected secret+configMap", openshift)
	}
}
