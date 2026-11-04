package controlplane

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func webAPIProbeOf(t *testing.T, tls simplyblockv1alpha2.ControlPlaneTLS) *corev1.Probe {
	t.Helper()
	probe := webAPIDeployment(aLocalControlPlane(tls)).Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe == nil {
		t.Fatal("the management API has no readiness probe, so the Service routes to pods that do not listen yet")
	}
	return probe
}

// Under mutual TLS the kubelet has no certificate to present, so the probe runs
// in the container and presents the pod's own.
func TestWebAPIReadinessUnderMutualTLSPresentsThePodCertificate(t *testing.T) {
	probe := webAPIProbeOf(t, simplyblockv1alpha2.ControlPlaneTLS{})

	if probe.Exec == nil {
		t.Fatalf("the probe is %+v; the kubelet cannot pass a client certificate", probe.ProbeHandler)
	}
	command := strings.Join(probe.Exec.Command, " ")
	for _, want := range []string{
		"--fail",
		"--cert " + tlsMountPath + "/tls.crt",
		"--key " + tlsMountPath + "/tls.key",
		"https://127.0.0.1:5000" + webAPIHealthPath,
	} {
		if !strings.Contains(command, want) {
			t.Errorf("probe command %q lacks %q", command, want)
		}
	}
	if !slices.Contains(probe.Exec.Command, "--max-time") {
		t.Errorf("probe command %q has no time limit of its own", command)
	}
}

// TLS without client certificates and plaintext leave the probe to the kubelet,
// on the scheme the API serves.
func TestWebAPIReadinessWithoutMutualTLSIsAnHTTPGet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tls    simplyblockv1alpha2.ControlPlaneTLS
		scheme corev1.URIScheme
	}{
		{"tls", simplyblockv1alpha2.ControlPlaneTLS{EnableMutualTLS: ptr.To(false)}, corev1.URISchemeHTTPS},
		{"plaintext", simplyblockv1alpha2.ControlPlaneTLS{EnableTLS: ptr.To(false)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := webAPIProbeOf(t, tc.tls)
			get := probe.HTTPGet
			if get == nil {
				t.Fatalf("the probe is %+v, not an HTTP GET", probe.ProbeHandler)
			}
			if get.Path != webAPIHealthPath || get.Port.IntValue() != webAPIPort || get.Scheme != tc.scheme {
				t.Errorf("the probe asks %s %s:%d, want %s %s:%d",
					get.Scheme, get.Path, get.Port.IntValue(), tc.scheme, webAPIHealthPath, webAPIPort)
			}
		})
	}
}

// A FoundationDB recovery of a few seconds must not take every replica out at
// once: one failure alone never makes a pod unready.
func TestWebAPIReadinessToleratesABriefDatabaseStall(t *testing.T) {
	probe := webAPIProbeOf(t, simplyblockv1alpha2.ControlPlaneTLS{})
	if probe.FailureThreshold < 2 {
		t.Errorf("a single failed probe takes the pod out (failureThreshold=%d)", probe.FailureThreshold)
	}
	if probe.TimeoutSeconds >= probe.PeriodSeconds {
		t.Errorf("the probe timeout (%ds) is not shorter than its period (%ds)", probe.TimeoutSeconds, probe.PeriodSeconds)
	}
}
