// Tests that a managed control plane runs its own Prometheus: the server and the
// Thanos sidecar that ships its blocks to the object store, the account the
// management API trusts for scrapes, and the scrape configuration.

package controlplane

import (
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
