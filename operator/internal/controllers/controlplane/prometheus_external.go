// The objects a control plane gives an external prometheus-operator in place of
// running its own Prometheus: a scraper identity, the monitors that scrape with
// it, and the alert rules. Selected by spec.source.local.observability.prometheus.mode.
// Built here rather than in the Helm chart so an OperatorHub install gets them too.

package controlplane

import (
	"context"
	_ "embed"
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	monitoringGroup = "monitoring.coreos.com"

	// metricsScraperServiceAccount is the identity every monitor presents to
	// the management API. Owned by the operator, so the management API can
	// trust it without knowing which Prometheus does the scraping.
	metricsScraperServiceAccount = "simplyblock-metrics-scraper"
	// metricsScraperTokenSecret holds that account's token. ScrapeConfig takes
	// credentials only from a Secret, not from a file.
	metricsScraperTokenSecret = "simplyblock-metrics-scraper-token"

	metricsScrapeConfigName = "simplyblock-webappapi-cluster-metrics"
	alertRuleName           = "simplyblock"
	scrapeInterval          = "30s"

	// spdkProxyServerName is the name the storage nodes' proxy certificate is
	// issued for, by sbcli's storage_deploy_spdk.yaml.j2.
	spdkProxyServerName = "simplyblock-spdk-proxy"

	// monitorSchemeHTTPS is ServiceMonitor and PodMonitor spelling; ScrapeConfig
	// takes "HTTPS".
	monitorSchemeHTTPS = "https"
)

var monitoringGVKs = []schema.GroupVersionKind{
	{Group: monitoringGroup, Version: "v1", Kind: "ServiceMonitor"},
	{Group: monitoringGroup, Version: "v1", Kind: "PodMonitor"},
	{Group: monitoringGroup, Version: "v1", Kind: "PrometheusRule"},
	{Group: monitoringGroup, Version: "v1alpha1", Kind: "ScrapeConfig"},
}

// externalPrometheus reports whether this control plane is scraped by an
// external prometheus-operator.
func externalPrometheus(cp *simplyblockv1alpha2.ControlPlane) bool {
	return cp.Spec.Source.Local.PrometheusMode() == simplyblockv1alpha2.PrometheusModeExternal
}

// metricsServiceAccounts is the account the management API trusts for
// scrapes: the managed Prometheus's own, or the scraper account the
// external monitors present.
func metricsServiceAccounts(cp *simplyblockv1alpha2.ControlPlane) string {
	account := prometheusServiceAccount
	if externalPrometheus(cp) {
		account = metricsScraperServiceAccount
	}
	return "system:serviceaccount:" + cp.Namespace + ":" + account
}

// prometheusObjectsFor is the managed Prometheus in Managed mode. In External
// mode it is the client certificate the monitors present under cert-manager
// mutual TLS, and the objects for the external prometheus-operator.
func prometheusObjectsFor(cp *simplyblockv1alpha2.ControlPlane) []client.Object {
	if !externalPrometheus(cp) {
		return prometheusObjects(cp)
	}
	var objects []client.Object
	if prometheusHasOwnCertificate(cp.Spec.Source.Local) {
		objects = append(objects, prometheusClientCertificateObject(cp.Namespace))
	}
	return append(objects, externalMonitoringObjects(cp)...)
}

// externalMonitoringObjects is the scraper identity, the monitors, and the
// alert rules. The account and its token come first: removeOtherPrometheusMode
// relies on that order.
func externalMonitoringObjects(cp *simplyblockv1alpha2.ControlPlane) []client.Object {
	ns := cp.Namespace
	return []client.Object{
		serviceAccount(ns, metricsScraperServiceAccount, nil),
		metricsScraperToken(ns),
		webAPIServiceMonitor(cp),
		webAPIScrapeConfig(cp),
		fdbExporterServiceMonitor(cp),
		rebalancerPodMonitor(cp),
		spdkProxyPodMonitor(cp),
		alertRule(cp),
	}
}

// requireMonitoringCRDs fails with a message naming prometheus-operator when
// External mode is set on a cluster that does not serve its kinds.
func requireMonitoringCRDs(mapper meta.RESTMapper) error {
	for _, gvk := range monitoringGVKs {
		if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			if meta.IsNoMatchError(err) {
				return fmt.Errorf("prometheus mode is External, but this cluster serves no %s %s: "+
					"install prometheus-operator, or set the mode to Managed", gvk.GroupVersion(), gvk.Kind)
			}
			return fmt.Errorf("looking up %s: %w", gvk, err)
		}
	}
	return nil
}

// removeOtherPrometheusMode deletes what the mode not in force applied: the
// managed Prometheus after a switch to External, and the scraper account and
// monitors after a switch to Managed. The StatefulSet's volume claim stays, as
// it does for any StatefulSet.
func (r *ControlPlaneReconciler) removeOtherPrometheusMode(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) error {
	ns := cp.Namespace
	if externalPrometheus(cp) {
		return r.deleteOwned(ctx, cp, []client.Object{
			&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: ComponentPrometheus, Namespace: ns}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ComponentPrometheus, Namespace: ns}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: prometheusHeadlessService, Namespace: ns}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: prometheusConfigName, Namespace: ns}},
			&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: prometheusServiceAccount, Namespace: ns}},
			&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: prometheusServiceAccount, Namespace: ns}},
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: prometheusServiceAccount, Namespace: ns}},
		})
	}
	stale := externalMonitoringObjects(cp)
	if requireMonitoringCRDs(r.RESTMapper()) != nil {
		// A cluster without the CRDs can hold only the account and its token.
		stale = stale[:2]
	}
	return r.deleteOwned(ctx, cp, stale)
}

// deleteOwned deletes each object that exists and is controlled by cp, and
// leaves alone one somebody else created.
func (r *ControlPlaneReconciler) deleteOwned(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane, objects []client.Object,
) error {
	for _, obj := range objects {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("read %s before deleting it: %w", obj.GetName(), err)
		}
		if !metav1.IsControlledBy(obj, cp) {
			continue
		}
		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete %s: %w", obj.GetName(), err)
		}
	}
	return nil
}

// metricsScraperToken asks the token controller for a long-lived token of the
// scraper account.
func metricsScraperToken(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        metricsScraperTokenSecret,
			Namespace:   namespace,
			Annotations: map[string]string{corev1.ServiceAccountNameKey: metricsScraperServiceAccount},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}
}

// monitoringObject is a prometheus-operator object with the spec's labels.
func monitoringObject(cp *simplyblockv1alpha2.ControlPlane, version, kind, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: monitoringGroup, Version: version, Kind: kind})
	u.SetName(name)
	u.SetNamespace(cp.Namespace)
	if labels := cp.Spec.Source.Local.ExternalPrometheusLabels(); len(labels) > 0 {
		u.SetLabels(maps.Clone(labels))
	}
	return u
}

// scraperAuthorization presents the scraper account's token.
func scraperAuthorization() map[string]any {
	return map[string]any{
		"type":        "Bearer",
		"credentials": map[string]any{"name": metricsScraperTokenSecret, "key": "token"},
	}
}

// monitorTLS is the TLS configuration a monitor scrapes a control-plane
// endpoint with: the issuer's CA and, under mutual TLS, the same client
// certificate the managed Prometheus presents.
func monitorTLS(local *simplyblockv1alpha2.LocalControlPlane, serverName string) map[string]any {
	tls := map[string]any{"serverName": serverName}
	if local.TLSProvider() == simplyblockv1alpha2.ControlPlaneTLSOpenShift {
		tls["ca"] = map[string]any{"configMap": map[string]any{"name": openShiftCAConfigMap, "key": openShiftCAKey}}
	} else {
		tls["ca"] = map[string]any{"secret": map[string]any{"name": ServingCertSecret, "key": "ca.crt"}}
	}
	if local.RequiresClientCertificate() {
		secret := ServingCertSecret
		if prometheusHasOwnCertificate(local) {
			secret = prometheusClientSecret
		}
		tls["cert"] = map[string]any{"secret": map[string]any{"name": secret, "key": "tls.crt"}}
		tls["keySecret"] = map[string]any{"name": secret, "key": "tls.key"}
	}
	return tls
}

// webAPIServiceMonitor scrapes every management API replica's own
// /_meta/metrics.
func webAPIServiceMonitor(cp *simplyblockv1alpha2.ControlPlane) *unstructured.Unstructured {
	local := cp.Spec.Source.Local
	endpoint := map[string]any{
		"port":          "http",
		"path":          "/_meta/metrics",
		"interval":      scrapeInterval,
		"scrapeTimeout": "30s",
		"authorization": scraperAuthorization(),
	}
	if local.ServesTLS() {
		endpoint["scheme"] = monitorSchemeHTTPS
		endpoint["tlsConfig"] = monitorTLS(local, ComponentWebAPI)
	}
	return monitoringObject(cp, "v1", "ServiceMonitor", ComponentWebAPI, map[string]any{
		"namespaceSelector": map[string]any{"matchNames": []any{cp.Namespace}},
		"selector":          map[string]any{"matchLabels": map[string]any{appLabel: ComponentWebAPI}},
		"endpoints":         []any{endpoint},
	})
}

// webAPIScrapeConfig scrapes the cluster-wide /api/v2/metrics once, through
// the Service, rather than once per replica: every replica answers it the
// same from shared state, and scraping each would multiply every count.
func webAPIScrapeConfig(cp *simplyblockv1alpha2.ControlPlane) *unstructured.Unstructured {
	local := cp.Spec.Source.Local
	spec := map[string]any{
		"staticConfigs": []any{map[string]any{
			"targets": []any{fmt.Sprintf("%s.%s.svc:%d", ComponentWebAPI, cp.Namespace, webAPIPort)},
		}},
		"metricsPath":    "/api/v2/metrics",
		"scrapeInterval": scrapeInterval,
		"scrapeTimeout":  "30s",
		"authorization":  scraperAuthorization(),
	}
	if local.ServesTLS() {
		spec["scheme"] = "HTTPS"
		spec["tlsConfig"] = monitorTLS(local, ComponentWebAPI)
	}
	return monitoringObject(cp, "v1alpha1", "ScrapeConfig", metricsScrapeConfigName, spec)
}

func fdbExporterServiceMonitor(cp *simplyblockv1alpha2.ControlPlane) *unstructured.Unstructured {
	return monitoringObject(cp, "v1", "ServiceMonitor", ComponentFDBExporter, map[string]any{
		"namespaceSelector": map[string]any{"matchNames": []any{cp.Namespace}},
		"selector":          map[string]any{"matchLabels": map[string]any{appLabel: ComponentFDBExporter}},
		"endpoints": []any{map[string]any{
			"port": "metrics", "path": "/metrics", "interval": scrapeInterval, "honorLabels": true,
		}},
	})
}

// storageNodeSelector matches the storage-node pods by the role label sbcli
// sets on them.
func storageNodeSelector(cp *simplyblockv1alpha2.ControlPlane) (map[string]any, map[string]any) {
	return map[string]any{"matchNames": []any{cp.Namespace}},
		map[string]any{"matchLabels": map[string]any{utils.LabelRole: utils.LabelSpdkProxyRole}}
}

// rebalancerPodMonitor scrapes the rebalancer sidecar. Pods without its port
// are dropped from the target list.
func rebalancerPodMonitor(cp *simplyblockv1alpha2.ControlPlane) *unstructured.Unstructured {
	namespaces, selector := storageNodeSelector(cp)
	return monitoringObject(cp, "v1", "PodMonitor", "simplyblock-rebalancer", map[string]any{
		"namespaceSelector": namespaces,
		"selector":          selector,
		"podMetricsEndpoints": []any{map[string]any{
			"port": "latency-metrics", "path": "/metrics", "interval": scrapeInterval,
		}},
	})
}

// spdkProxyPodMonitor scrapes the storage-node proxy's /_meta/metrics, on the
// port it serves RPCs on. Mutual TLS is what gates it.
func spdkProxyPodMonitor(cp *simplyblockv1alpha2.ControlPlane) *unstructured.Unstructured {
	local := cp.Spec.Source.Local
	namespaces, selector := storageNodeSelector(cp)
	endpoint := map[string]any{"port": "proxy-http", "path": "/_meta/metrics", "interval": scrapeInterval}
	if local.ServesTLS() {
		endpoint["scheme"] = monitorSchemeHTTPS
		endpoint["tlsConfig"] = monitorTLS(local, spdkProxyServerName)
	}
	return monitoringObject(cp, "v1", "PodMonitor", "simplyblock-spdk-proxy", map[string]any{
		"namespaceSelector":   namespaces,
		"selector":            selector,
		"podMetricsEndpoints": []any{endpoint},
	})
}

//go:embed manifests/alert-rules.yaml
var alertRulesYAML []byte

// alertRule is the alert rules as one PrometheusRule group. The routing to
// Alertmanager receivers is left to whoever runs that Alertmanager.
func alertRule(cp *simplyblockv1alpha2.ControlPlane) *unstructured.Unstructured {
	var rules []any
	if err := yaml.Unmarshal(alertRulesYAML, &rules); err != nil {
		panic(fmt.Sprintf("manifests/alert-rules.yaml does not parse: %v", err))
	}
	return monitoringObject(cp, "v1", "PrometheusRule", alertRuleName, map[string]any{
		"groups": []any{map[string]any{"name": alertRuleName, "rules": rules}},
	})
}
