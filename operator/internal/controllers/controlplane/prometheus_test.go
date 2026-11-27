// Tests that a managed control plane runs its Prometheus through the
// prometheus-operator, scraping what the install runs, and that an upgrade
// copies the old Prometheus's data before replacing it.

package controlplane

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"

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

func findUnstructured(t *testing.T, objects []client.Object, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, obj := range objects {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == kind && u.GetName() == name {
			return u
		}
	}
	t.Fatalf("no %s %s in the install", kind, name)
	return nil
}

// Prometheus runs through the prometheus-operator: a Prometheus resource under
// the account the management API trusts, and one monitor per scrape job.
func TestTheInstallRunsPrometheusThroughThePrometheusOperator(t *testing.T) {
	objects := managementAPIObjects(localControlPlane())
	for _, obj := range objects {
		if _, ok := obj.(*appsv1.StatefulSet); ok && obj.GetName() == ComponentPrometheus {
			t.Error("the install still carries its own Prometheus StatefulSet")
		}
	}

	prom := findUnstructured(t, objects, "Prometheus", "simplyblock")
	if sa, _, _ := unstructured.NestedString(prom.Object, "spec", "serviceAccountName"); sa != prometheusServiceAccount {
		t.Errorf("Prometheus runs as %q, want %q", sa, prometheusServiceAccount)
	}
	if r, _, _ := unstructured.NestedString(prom.Object, "spec", "retention"); r != "15d" {
		t.Errorf("retention = %q, want 15d, the retention Prometheus had before", r)
	}
	if f, _, _ := unstructured.NestedString(prom.Object, "spec", "thanos", "objectStorageConfigFile"); f == "" {
		t.Error("the Thanos sidecar names no object store configuration")
	}
	svc := findTyped[*corev1.Service](t, objects, prometheusHost)
	podLabels, _, _ := unstructured.NestedStringMap(prom.Object, "spec", "podMetadata", "labels")
	for k, v := range svc.Spec.Selector {
		if podLabels[k] != v {
			t.Errorf("Service %s selects %s=%s, which the Prometheus pods do not carry", prometheusHost, k, v)
		}
	}

	scrape := findUnstructured(t, objects, "ScrapeConfig", "simplyblock-metrics")
	targets, _, _ := unstructured.NestedSlice(scrape.Object, "spec", "staticConfigs")
	if len(targets) != 1 || !strings.Contains(fmt.Sprint(targets), "simplyblock-webappapi:5000") {
		t.Errorf("staticConfigs = %v, want the management API Service once", targets)
	}
	token, _, _ := unstructured.NestedString(scrape.Object, "spec", "authorization", "credentials", "name")
	secret := findTyped[*corev1.Secret](t, objects, token)
	if secret.Type != corev1.SecretTypeServiceAccountToken ||
		secret.Annotations[corev1.ServiceAccountNameKey] != prometheusServiceAccount {
		t.Errorf("token Secret %s is %s for %q, want a token of %s",
			token, secret.Type, secret.Annotations[corev1.ServiceAccountNameKey], prometheusServiceAccount)
	}
	findUnstructured(t, objects, "ServiceMonitor", ComponentFDBExporter)
	findUnstructured(t, objects, "PodMonitor", "simplyblock-rebalancer-probe")
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

	scrape := findUnstructured(t, objects, "ScrapeConfig", "simplyblock-metrics")
	if got, _, _ := unstructured.NestedString(scrape.Object, "spec", "tlsConfig", "cert", "secret", "name"); got != prometheusClientSecret {
		t.Errorf("Prometheus presents the certificate of Secret %q, want its own %s", got, prometheusClientSecret)
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

// monitoringClient serves the prometheus-operator kinds, as a cluster with the
// prometheus-operator installed does.
func monitoringClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{prometheusGVK, scrapeConfigGVK, serviceMonitorGVK, podMonitorGVK} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithRESTMapper(mapper).WithObjects(objects...).Build()
}

func prometheusApplied(t *testing.T, c client.Client) bool {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(prometheusGVK)
	return c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "simplyblock"}, u) == nil
}

// An upgrade stops the old Prometheus, copies its volume into the one the new
// Prometheus claims, and starts the new one only once the copy succeeded.
func TestAnUpgradeCopiesTheOldPrometheusBeforeReplacingIt(t *testing.T) {
	ctx := context.Background()
	cp := localControlPlane()
	oldClaim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-volume-simplyblock-prometheus-0", Namespace: testNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To("local-path"),
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("20Gi"),
			}},
		},
	}
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-prometheus-0", Namespace: testNamespace}}
	oldSet := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: ComponentPrometheus, Namespace: testNamespace}}
	c := monitoringClient(t, cp, oldClaim, oldPod, oldSet)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}
	apply := func() {
		t.Helper()
		if err := r.applyEverything(ctx, cp); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}

	apply()
	if err := c.Get(ctx, client.ObjectKeyFromObject(oldSet), &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Errorf("the old StatefulSet is still there (%v), so the old Prometheus keeps writing", err)
	}
	var newClaim corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "prometheus-simplyblock-db-prometheus-simplyblock-0"}, &newClaim); err != nil {
		t.Fatalf("the new Prometheus's claim was not created: %v", err)
	}
	if newClaim.Spec.StorageClassName == nil || *newClaim.Spec.StorageClassName != "local-path" {
		t.Errorf("the new claim's class is %v, want the old claim's", newClaim.Spec.StorageClassName)
	}
	job := &batchv1.Job{}
	jobKey := client.ObjectKey{Namespace: testNamespace, Name: "simplyblock-prometheus-copy"}
	if err := c.Get(ctx, jobKey, job); err == nil {
		t.Error("the copy started while the old Prometheus pod still runs")
	}

	if err := c.Delete(ctx, oldPod); err != nil {
		t.Fatal(err)
	}
	apply()
	if err := c.Get(ctx, jobKey, job); err != nil {
		t.Fatalf("no copy Job once the old pod is gone: %v", err)
	}
	if prometheusApplied(t, c) {
		t.Error("the new Prometheus started before the copy finished")
	}

	job.Status.Succeeded = 1
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	apply()
	if !prometheusApplied(t, c) {
		t.Error("the new Prometheus was not started after the copy succeeded")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(oldClaim), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Errorf("the old claim was removed: %v", err)
	}
}
