// Tests for spec.source.local.observability.prometheus.mode: which Prometheus
// the management API trusts, which objects each mode applies, and what a switch
// from Managed to External removes.

package controlplane

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// externalControlPlane is a local control plane scraped by an external
// prometheus-operator.
func externalControlPlane(labels map[string]string) *simplyblockv1alpha2.ControlPlane {
	cp := localControlPlane()
	cp.Spec.Source.Local.Observability = &simplyblockv1alpha2.ControlPlaneObservability{
		Prometheus: &simplyblockv1alpha2.ControlPlanePrometheus{
			Mode:     simplyblockv1alpha2.PrometheusModeExternal,
			External: &simplyblockv1alpha2.ExternalPrometheus{Labels: labels},
		},
	}
	return cp
}

// monitoringMapper knows the prometheus-operator kinds, as a cluster with
// prometheus-operator installed does.
func monitoringMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range monitoringGVKs {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return mapper
}

func findUnstructured(t *testing.T, objects []client.Object, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, obj := range objects {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == kind && u.GetName() == name {
			return u
		}
	}
	t.Fatalf("no %s named %q among the built objects", kind, name)
	return nil
}

// Managed is the default, and its Prometheus scrapes as simplyblock-prometheus.
// Without that account in the list every scrape gets a 401.
func TestManagedPrometheusIsTrustedAsAMetricsScraper(t *testing.T) {
	cp := localControlPlane()

	api := findDeployment(t, managementAPIObjects(cp), ComponentWebAPI)
	env := findEnvVar(t, api, "SB_K8S_METRICS_SERVICE_ACCOUNTS")

	want := "system:serviceaccount:" + testNamespace + ":" + prometheusServiceAccount
	if env.Value != want {
		t.Errorf("SB_K8S_METRICS_SERVICE_ACCOUNTS = %q, want %q", env.Value, want)
	}
}

// External trusts the scraper account the operator creates, whose token the
// monitors present, and not the managed Prometheus that no longer runs.
func TestExternalPrometheusTrustsTheScraperAccount(t *testing.T) {
	cp := externalControlPlane(nil)

	api := findDeployment(t, managementAPIObjects(cp), ComponentWebAPI)
	env := findEnvVar(t, api, "SB_K8S_METRICS_SERVICE_ACCOUNTS")

	want := "system:serviceaccount:" + testNamespace + ":" + metricsScraperServiceAccount
	if env.Value != want {
		t.Errorf("SB_K8S_METRICS_SERVICE_ACCOUNTS = %q, want %q", env.Value, want)
	}
}

// External runs no Prometheus of its own, but keeps the client certificate the
// monitors present under cert-manager mutual TLS.
func TestExternalPrometheusDropsTheStatefulSetButKeepsTheClientCertificate(t *testing.T) {
	cp := externalControlPlane(nil)

	objects := managementAPIObjects(cp)
	for _, obj := range objects {
		if _, ok := obj.(*appsv1.StatefulSet); ok && obj.GetName() == ComponentPrometheus {
			t.Errorf("External mode still builds the %s StatefulSet", ComponentPrometheus)
		}
	}
	findUnstructured(t, objects, "Certificate", prometheusClientCertificate)
}

// The ScrapeConfig authenticates with a Secret-backed token. ScrapeConfig's
// SafeAuthorization has no credentialsFile field, so a file path is dropped or
// refused by the apiserver and every scrape gets a 401.
func TestExternalScrapeConfigAuthenticatesWithASecret(t *testing.T) {
	cp := externalControlPlane(nil)

	sc := findUnstructured(t, managementAPIObjects(cp), "ScrapeConfig", metricsScrapeConfigName)

	if _, found, _ := unstructured.NestedFieldNoCopy(sc.Object, "spec", "authorization", "credentialsFile"); found {
		t.Error("ScrapeConfig sets authorization.credentialsFile, which the CRD does not have")
	}
	name, _, _ := unstructured.NestedString(sc.Object, "spec", "authorization", "credentials", "name")
	key, _, _ := unstructured.NestedString(sc.Object, "spec", "authorization", "credentials", "key")
	if name != metricsScraperTokenSecret || key != "token" {
		t.Errorf("authorization.credentials = %s/%s, want %s/token", name, key, metricsScraperTokenSecret)
	}

	token := findSecret(t, managementAPIObjects(cp), metricsScraperTokenSecret)
	if token.Type != corev1.SecretTypeServiceAccountToken ||
		token.Annotations[corev1.ServiceAccountNameKey] != metricsScraperServiceAccount {
		t.Errorf("token Secret = %s for %q, want a service-account token for %s",
			token.Type, token.Annotations[corev1.ServiceAccountNameKey], metricsScraperServiceAccount)
	}
}

// Every object an external prometheus-operator selects carries the labels from
// the spec, so a Prometheus with a narrow selector still finds them.
func TestExternalMonitoringObjectsCarryTheSpecLabels(t *testing.T) {
	cp := externalControlPlane(map[string]string{"release": "kube-prometheus-stack"})

	objects := managementAPIObjects(cp)
	for _, kind := range []string{"ServiceMonitor", "PodMonitor", "ScrapeConfig", "PrometheusRule"} {
		seen := false
		for _, obj := range objects {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok || u.GetKind() != kind {
				continue
			}
			seen = true
			if got := u.GetLabels()["release"]; got != "kube-prometheus-stack" {
				t.Errorf("%s %s labels[release] = %q, want kube-prometheus-stack", kind, u.GetName(), got)
			}
		}
		if !seen {
			t.Errorf("External mode builds no %s", kind)
		}
	}
}

// Managed builds none of the prometheus-operator kinds: a cluster without
// their CRDs could not apply them.
func TestManagedPrometheusBuildsNoMonitoringObjects(t *testing.T) {
	for _, obj := range managementAPIObjects(localControlPlane()) {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind().Group == monitoringGroup {
			t.Errorf("Managed mode builds %s %s", u.GetKind(), u.GetName())
		}
	}
}

// The rules match the Grafana alerting rules by name, so an alert keeps its
// name when a deployment moves from Managed to External.
func TestExternalPrometheusRuleCarriesEveryAlert(t *testing.T) {
	rule := findUnstructured(t, managementAPIObjects(externalControlPlane(nil)), "PrometheusRule", alertRuleName)

	groups, _, _ := unstructured.NestedSlice(rule.Object, "spec", "groups")
	if len(groups) != 1 {
		t.Fatalf("PrometheusRule has %d groups, want 1", len(groups))
	}
	rules, _, _ := unstructured.NestedSlice(groups[0].(map[string]any), "rules")
	got := map[string]bool{}
	for _, r := range rules {
		got[r.(map[string]any)["alert"].(string)] = true
	}
	for _, name := range []string{
		"Device_status_online_to_unavailable", "Device_status_online_to_read_only",
		"Cluster_status_online_to_degraded", "Cluster_status_online_to_suspended",
		"StorageNode_status_online_to_unreachable", "StorageNode_status_online_to_down",
		"StorageNode_health_check_false", "Cluster_provisioned_capacity_reached",
		"Cluster_absolute_capacity_reached", "Lvol_status_online_to_offline",
		"RootFilesystemLowSpace",
	} {
		if !got[name] {
			t.Errorf("PrometheusRule has no alert %s", name)
		}
	}
}

// Switching an installed control plane from Managed to External removes the
// Prometheus the operator applied before. Leaving it out of the apply alone
// keeps it running, and the management API is scraped twice.
func TestSwitchingToExternalRemovesTheManagedPrometheus(t *testing.T) {
	cp := localControlPlane()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRESTMapper(monitoringMapper()).
		WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}
	ctx := context.Background()

	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("apply Managed: %v", err)
	}
	key := client.ObjectKey{Namespace: testNamespace, Name: ComponentPrometheus}
	if err := c.Get(ctx, key, &appsv1.StatefulSet{}); err != nil {
		t.Fatalf("Managed did not apply the Prometheus StatefulSet: %v", err)
	}

	cp.Spec.Source.Local.Observability = externalControlPlane(nil).Spec.Source.Local.Observability
	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("apply External: %v", err)
	}

	if err := c.Get(ctx, key, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Errorf("Get the Prometheus StatefulSet after the switch: %v, want NotFound", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: prometheusConfigName},
		&corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("Get the Prometheus ConfigMap after the switch: %v, want NotFound", err)
	}
}

// A managed Prometheus somebody else created is not the operator's to delete.
func TestSwitchingToExternalLeavesAnUnownedPrometheus(t *testing.T) {
	cp := externalControlPlane(nil)
	foreign := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: ComponentPrometheus, Namespace: testNamespace}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRESTMapper(monitoringMapper()).
		WithObjects(cp, foreign).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.applyEverything(context.Background(), cp); err != nil {
		t.Fatalf("apply External: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &appsv1.StatefulSet{}); err != nil {
		t.Errorf("an unowned StatefulSet was removed: %v", err)
	}
}

// External on a cluster without the prometheus-operator CRDs fails with an
// error naming the missing kind, rather than an apply error about an unknown
// resource.
func TestExternalWithoutTheMonitoringCRDsSaysSo(t *testing.T) {
	cp := externalControlPlane(nil)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRESTMapper(meta.NewDefaultRESTMapper(nil)).
		WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	err := r.applyEverything(context.Background(), cp)
	if err == nil || !strings.Contains(err.Error(), "prometheus-operator") {
		t.Errorf("apply External without the CRDs: %v, want an error naming prometheus-operator", err)
	}
}

// external is only meaningful with mode External, and a mode outside the enum
// is refused.
func TestControlPlaneCELPrometheusMode(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name       string
		prometheus *simplyblockv1alpha2.ControlPlanePrometheus
		wantReason string
	}{
		{name: "managed", prometheus: &simplyblockv1alpha2.ControlPlanePrometheus{Mode: simplyblockv1alpha2.PrometheusModeManaged}},
		{name: "mode left out defaults to managed", prometheus: &simplyblockv1alpha2.ControlPlanePrometheus{}},
		{name: "external with labels", prometheus: &simplyblockv1alpha2.ControlPlanePrometheus{
			Mode:     simplyblockv1alpha2.PrometheusModeExternal,
			External: &simplyblockv1alpha2.ExternalPrometheus{Labels: map[string]string{"release": "x"}},
		}},
		{name: "external block under managed", prometheus: &simplyblockv1alpha2.ControlPlanePrometheus{
			Mode:     simplyblockv1alpha2.PrometheusModeManaged,
			External: &simplyblockv1alpha2.ExternalPrometheus{},
		}, wantReason: "external is only valid when mode is External"},
		{name: "unknown mode", prometheus: &simplyblockv1alpha2.ControlPlanePrometheus{Mode: "Off"},
			wantReason: "Unsupported value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := &simplyblockv1alpha2.ControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: SingletonName, Namespace: freshNamespace(t, apiClient)},
				Spec: simplyblockv1alpha2.ControlPlaneSpec{Source: simplyblockv1alpha2.ControlPlaneSource{
					Local: &simplyblockv1alpha2.LocalControlPlane{
						Image:         testImage,
						Observability: &simplyblockv1alpha2.ControlPlaneObservability{Prometheus: tc.prometheus},
					},
				}},
			}
			err := apiClient.Create(context.Background(), cp)
			switch {
			case tc.wantReason == "" && err != nil:
				t.Fatalf("a legal block was denied: %v", err)
			case tc.wantReason != "" && err == nil:
				t.Fatal("an illegal block was admitted")
			case tc.wantReason != "" && !strings.Contains(err.Error(), tc.wantReason):
				t.Errorf("denied with %q, want %q", err, tc.wantReason)
			}
		})
	}
}

func findSecret(t *testing.T, objects []client.Object, name string) *corev1.Secret {
	t.Helper()
	for _, obj := range objects {
		if s, ok := obj.(*corev1.Secret); ok && s.GetName() == name {
			return s
		}
	}
	t.Fatalf("no Secret named %q among the built objects", name)
	return nil
}

// Switching back to Managed removes the monitors and the scraper account, so
// the external Prometheus stops scraping beside the managed one.
func TestSwitchingToManagedRemovesTheExternalMonitors(t *testing.T) {
	cp := externalControlPlane(nil)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRESTMapper(monitoringMapper()).
		WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}
	ctx := context.Background()

	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("apply External: %v", err)
	}
	cp.Spec.Source.Local.Observability = nil
	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("apply Managed: %v", err)
	}

	for _, obj := range externalMonitoringObjects(externalControlPlane(nil)) {
		live := obj.DeepCopyObject().(client.Object)
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), live); !apierrors.IsNotFound(err) {
			t.Errorf("Get %T %s after switching to Managed: %v, want NotFound", obj, obj.GetName(), err)
		}
	}
}
