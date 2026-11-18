// Tests that a managed control plane runs its own Prometheus: the server and the
// Thanos sidecar that ships its blocks to the object store, the account the
// management API trusts for scrapes, and the scrape configuration.

package controlplane

import (
	"os"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func findTyped[T client.Object](t *testing.T, objects []client.Object, name string) T {
	t.Helper()
	for _, obj := range objects {
		if typed, ok := obj.(T); ok && obj.GetName() == name {
			return typed
		}
	}
	var zero T
	t.Fatalf("no %T named %s in the install", zero, name)
	return zero
}

// The install runs Prometheus under the Service name every control-plane
// container is told to push to, with the Thanos sidecar beside it.
func TestTheInstallRunsPrometheusWhereTheControlPlanePushes(t *testing.T) {
	objects := managementAPIObjects(localControlPlane())

	svc := findTyped[*corev1.Service](t, objects, prometheusHost)
	ports := map[int32]bool{}
	for _, p := range svc.Spec.Ports {
		ports[p.Port] = true
	}
	for _, want := range []int32{9090, 10901, 10902} {
		if !ports[want] {
			t.Errorf("Service %s does not publish port %d", prometheusHost, want)
		}
	}

	sts := findTyped[*appsv1.StatefulSet](t, objects, ComponentPrometheus)
	spec := sts.Spec.Template.Spec
	if spec.ServiceAccountName != prometheusServiceAccount {
		t.Errorf("Prometheus runs as %q, want %q, the account the management API trusts for scrapes",
			spec.ServiceAccountName, prometheusServiceAccount)
	}
	names := map[string]corev1.Container{}
	for _, c := range spec.Containers {
		names[c.Name] = c
	}
	if _, ok := names["prometheus"]; !ok {
		t.Error("no prometheus container")
	}
	sidecar, ok := names["thanos-sidecar"]
	if !ok {
		t.Fatal("no thanos-sidecar container: the object store would never receive a block")
	}
	if !strings.Contains(strings.Join(sidecar.Args, " "), "--objstore.config-file=") {
		t.Errorf("sidecar args %v name no object store configuration", sidecar.Args)
	}
	mountsObjstore := false
	for _, v := range spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == objectStoreConfigName {
			mountsObjstore = true
		}
	}
	if !mountsObjstore {
		t.Errorf("Prometheus does not mount ConfigMap %s", objectStoreConfigName)
	}
	if len(sts.Spec.VolumeClaimTemplates) != 1 || sts.Spec.VolumeClaimTemplates[0].Name != prometheusDataVolume {
		t.Errorf("claim templates = %v, want one named %s", sts.Spec.VolumeClaimTemplates, prometheusDataVolume)
	}
}

// Prometheus scrapes the management API, the database exporter, and the
// rebalancer probes, and its account can discover the probe pods.
func TestPrometheusScrapesWhatTheInstallRuns(t *testing.T) {
	objects := managementAPIObjects(localControlPlane())

	cm := findTyped[*corev1.ConfigMap](t, objects, prometheusConfigName)
	config := cm.Data["prometheus.yml"]
	for _, want := range []string{
		"simplyblock-webappapi:5000", "/api/v2/metrics",
		"simplyblock-fdb-exporter:9444", "simplyblock_rebalancer_probe",
		"credentials_file: /var/run/secrets/kubernetes.io/serviceaccount/token",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("prometheus.yml has no %q:\n%s", want, config)
		}
	}

	role := findTyped[*rbacv1.Role](t, objects, prometheusServiceAccount)
	listsPods := false
	for _, rule := range role.Rules {
		for _, r := range rule.Resources {
			if r == "pods" && strings.Contains(strings.Join(rule.Verbs, ","), "list") {
				listsPods = true
			}
		}
	}
	if !listsPods {
		t.Error("Prometheus's Role cannot list pods, so pod discovery finds no rebalancer probe")
	}
	findTyped[*rbacv1.RoleBinding](t, objects, prometheusServiceAccount)
	findTyped[*corev1.ServiceAccount](t, objects, prometheusServiceAccount)
}

// A management API that serves TLS is scraped over HTTPS against the CA, and
// with a client certificate when it demands one.
func TestPrometheusScrapesOverTLSWhenTheAPIServesIt(t *testing.T) {
	cp := localControlPlane()
	if !cp.Spec.Source.Local.ServesTLS() || !cp.Spec.Source.Local.RequiresClientCertificate() {
		t.Fatal("the default control plane is expected to serve mutual TLS")
	}
	config := findTyped[*corev1.ConfigMap](t, managementAPIObjects(cp), prometheusConfigName).Data["prometheus.yml"]
	for _, want := range []string{"scheme: https", "ca_file: ", "cert_file: ", "server_name: simplyblock-webappapi"} {
		if !strings.Contains(config, want) {
			t.Errorf("prometheus.yml has no %q with TLS on:\n%s", want, config)
		}
	}

	off := false
	cp.Spec.Source.Local.TLS = simplyblockv1alpha2.ControlPlaneTLS{EnableTLS: &off}
	if cp.Spec.Source.Local.ServesTLS() {
		t.Fatal("the plaintext fixture still serves TLS")
	}
	plain := findTyped[*corev1.ConfigMap](t, managementAPIObjects(cp), prometheusConfigName).Data["prometheus.yml"]
	if strings.Contains(plain, "scheme: https") {
		t.Errorf("a plaintext management API is scraped over https:\n%s", plain)
	}
}

// Prometheus is watched, and a Prometheus at zero degrades the control plane
// rather than making it unavailable.
func TestPrometheusIsANonEssentialComponent(t *testing.T) {
	for _, comp := range componentTable {
		if comp.name == ComponentPrometheus {
			if comp.essential || comp.kind != kindStatefulSet {
				t.Errorf("Prometheus is %+v, want a non-essential StatefulSet", comp)
			}
			return
		}
	}
	t.Error("Prometheus is not in the component table")
}

// Both images are pinned and listed for mirroring.
func TestPrometheusImagesAreListedForMirroring(t *testing.T) {
	raw, err := os.ReadFile("../../prerequisites/related-images.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{prometheusImage, thanosImage} {
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("%s is not pinned by digest", ref)
		}
		if !strings.Contains(string(raw), ref) {
			t.Errorf("%s is missing from related-images.yaml", ref)
		}
	}
}

// Thanos Query reaches each replica's sidecar by its stable DNS name, which
// needs the StatefulSet governed by a headless Service of the chart's name.
func TestPrometheusReplicasHaveTheNamesThanosQueryDials(t *testing.T) {
	objects := managementAPIObjects(localControlPlane())
	headless := findTyped[*corev1.Service](t, objects, prometheusHeadlessService)
	if headless.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("%s has clusterIP %q, want None", prometheusHeadlessService, headless.Spec.ClusterIP)
	}
	if sts := findTyped[*appsv1.StatefulSet](t, objects, ComponentPrometheus); sts.Spec.ServiceName != prometheusHeadlessService {
		t.Errorf("StatefulSet serviceName = %q, want %q", sts.Spec.ServiceName, prometheusHeadlessService)
	}
}

// Regression: 2026-11-10-prometheus-borrowed-identity. Under mutual TLS every
// client presents its own certificate, and Prometheus presented the management
// API's serving certificate, so it held the server's key and the server could
// not tell its scraper from itself.
func TestPrometheusPresentsItsOwnClientCertificate(t *testing.T) {
	cp := aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{})
	if !cp.Spec.Source.Local.RequiresClientCertificate() {
		t.Fatal("the default control plane is expected to require client certificates")
	}
	objects := managementAPIObjects(cp)

	spec := findTyped[*appsv1.StatefulSet](t, objects, ComponentPrometheus).Spec.Template.Spec
	mountsClient := false
	for _, v := range spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == ServingCertSecret {
			t.Errorf("Prometheus mounts the serving Secret %s in volume %s", ServingCertSecret, v.Name)
		}
		if v.Secret != nil && v.Secret.SecretName == prometheusClientSecret {
			mountsClient = true
		}
	}
	if !mountsClient {
		t.Errorf("Prometheus does not mount its client Secret %s", prometheusClientSecret)
	}

	var cert *unstructured.Unstructured
	for _, obj := range objects {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "Certificate" &&
			u.GetName() == prometheusClientCertificate {
			cert = u
		}
	}
	if cert == nil {
		t.Fatalf("no Certificate %s in the install", prometheusClientCertificate)
	}
	if got, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName"); got != prometheusClientSecret {
		t.Errorf("the Certificate issues into %q, want %q", got, prometheusClientSecret)
	}
	usages, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "usages")
	if !slices.Contains(usages, "client auth") || slices.Contains(usages, "server auth") {
		t.Errorf("the Certificate's usages are %v, want client auth without server auth", usages)
	}
}

// Without mutual TLS there is nothing to present, so no client Certificate is
// issued. The OpenShift service CA signs serving certificates only, so that
// provider keeps the shared volume until #863 settles it.
func TestPrometheusClientCertificateOnlyUnderCertManagerMutualTLS(t *testing.T) {
	off := false
	for name, tls := range map[string]simplyblockv1alpha2.ControlPlaneTLS{
		"no mutual TLS": {EnableMutualTLS: &off},
		"OpenShift":     {Provider: simplyblockv1alpha2.ControlPlaneTLSOpenShift},
	} {
		for _, obj := range managementAPIObjects(aLocalControlPlane(tls)) {
			if obj.GetName() == prometheusClientCertificate {
				t.Errorf("%s: the install carries %T %s", name, obj, prometheusClientCertificate)
			}
		}
	}
}
