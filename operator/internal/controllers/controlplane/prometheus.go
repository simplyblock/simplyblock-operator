// The Prometheus a managed control plane runs through the prometheus-operator,
// applied with the management API: a Prometheus resource, one monitor per scrape
// job, and the Thanos sidecar that uploads each two-hour block to the object
// store the datastore step created.

package controlplane

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// The prometheus-operator kinds the install applies.
var (
	prometheusGVK     = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "Prometheus"}
	serviceMonitorGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}
	podMonitorGVK     = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PodMonitor"}
	scrapeConfigGVK   = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1alpha1", Kind: "ScrapeConfig"}
)

const (
	prometheusImage = "quay.io/simplyblock-io/prometheus:v2.51.0@sha256:5ccad477d0057e62a7cd1981ffcc43785ac10c5a35522dc207466ff7e7ec845f"
	thanosImage     = "quay.io/simplyblock-io/thanos:v0.31.0@sha256:e7d337d6ac2aea3f0f9314ec9830291789e16e2b480b9d353be02d05ce7f2a7e"

	// prometheusServiceAccount is the account the management API trusts for
	// scrapes, through SB_K8S_METRICS_SERVICE_ACCOUNTS.
	prometheusServiceAccount = "simplyblock-prometheus"
	// prometheusTokenSecret holds the account's token, which the ScrapeConfig
	// presents: the kind takes credentials from a Secret only.
	prometheusTokenSecret = "simplyblock-prometheus-token"

	// prometheusName is the Prometheus resource. The prometheus-operator derives
	// its StatefulSet's name and its claim's name from it.
	prometheusName           = "simplyblock"
	prometheusStatefulSet    = "prometheus-" + prometheusName
	prometheusClaim          = "prometheus-" + prometheusName + "-db-" + prometheusStatefulSet + "-0"
	prometheusVolumeSize     = "20Gi"
	prometheusRetention      = "15d"
	prometheusObjstoreVolume = "objstore-config"

	// scrapedByLabel marks the monitors this Prometheus selects.
	scrapedByLabel = "storage.simplyblock.io/scraped-by"

	// prometheusClientCertificate and its Secret are the names the chart issued
	// them under, so an install that came from the chart already has the Secret.
	prometheusClientCertificate = "simplyblock-prometheus-client"
	prometheusClientSecret      = "simplyblock-prometheus-client-tls"
	thanosGRPCPort              = 10901
	thanosHTTPPort              = 10902
)

// The Prometheus a 26.3 install ran as a StatefulSet of its own, replaced on
// upgrade (prometheus_migration.go).
const (
	legacyPrometheusClaim    = "storage-volume-" + ComponentPrometheus + "-0"
	legacyPrometheusPod      = ComponentPrometheus + "-0"
	legacyPrometheusConfig   = "simplyblock-prometheus-config"
	legacyPrometheusHeadless = "simplyblock-prometheus-headless"
)

// prometheusObjects is the account, its token and discovery grant, the
// monitors, the Prometheus resource, and its Service.
func prometheusObjects(cp *simplyblockv1alpha2.ControlPlane) []client.Object {
	ns := cp.Namespace
	objects := []client.Object{
		serviceAccount(ns, prometheusServiceAccount, nil),
		prometheusToken(ns),
		prometheusRole(ns),
		prometheusRoleBinding(ns),
		prometheusScrapeConfig(cp),
		fdbExporterMonitor(ns),
		rebalancerProbeMonitor(ns),
		prometheusResource(cp),
		prometheusService(ns),
	}
	if prometheusHasOwnCertificate(cp.Spec.Source.Local) {
		objects = append(objects, prometheusClientCertificateObject(ns))
	}
	return objects
}

// prometheusHasOwnCertificate says whether Prometheus presents a client
// certificate of its own. The OpenShift service CA signs serving certificates
// only, so that provider still presents the serving one (#863).
func prometheusHasOwnCertificate(local *simplyblockv1alpha2.LocalControlPlane) bool {
	return local.RequiresClientCertificate() &&
		local.TLSProvider() == simplyblockv1alpha2.ControlPlaneTLSCertManager
}

// prometheusClientCertificateObject is Prometheus's client identity under
// mutual TLS: client auth only, so it cannot serve as the management API.
func prometheusClientCertificateObject(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"name": prometheusClientCertificate, "namespace": namespace},
		"spec": map[string]any{
			"commonName": prometheusServiceAccount,
			"secretName": prometheusClientSecret,
			"issuerRef": map[string]any{
				"kind": "ClusterIssuer",
				"name": utils.CertManagerClusterIssuerName,
			},
			"usages": []any{"digital signature", "key encipherment", "client auth"},
		},
	}}
}

// prometheusRole lets the server discover the monitors' targets in its own
// namespace and nothing else.
func prometheusRole(namespace string) *rbacv1.Role {
	read := []string{"get", "list", "watch"}
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: prometheusServiceAccount, Namespace: namespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods", "services", "endpoints"}, Verbs: read},
			{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: read},
		},
	}
}

func prometheusRoleBinding(namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: prometheusServiceAccount, Namespace: namespace},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: prometheusServiceAccount,
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: prometheusServiceAccount, Namespace: namespace,
		}},
	}
}

// prometheusToken is a long-lived token of the account the management API
// trusts for scrapes. Kubernetes fills it in.
func prometheusToken(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: prometheusTokenSecret, Namespace: namespace,
			Annotations: map[string]string{corev1.ServiceAccountNameKey: prometheusServiceAccount},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}
}

// monitoringObject is a prometheus-operator object this Prometheus selects.
func monitoringObject(gvk schema.GroupVersionKind, namespace, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetLabels(map[string]string{scrapedByLabel: prometheusName})
	return u
}

// prometheusScrapeConfig scrapes the management API through its Service, once.
// A ServiceMonitor would scrape every replica, and each serves the same
// cluster-wide series.
func prometheusScrapeConfig(cp *simplyblockv1alpha2.ControlPlane) *unstructured.Unstructured {
	local := cp.Spec.Source.Local
	spec := map[string]any{
		"jobName":        "simplyblock_metrics",
		"staticConfigs":  []any{map[string]any{"targets": []any{ComponentWebAPI + ":5000"}}},
		"metricsPath":    "/api/v2/metrics",
		"scrapeInterval": "30s",
		"scrapeTimeout":  "30s",
		"authorization": map[string]any{
			"type":        "Bearer",
			"credentials": map[string]any{"name": prometheusTokenSecret, "key": corev1.ServiceAccountTokenKey},
		},
	}
	if local.ServesTLS() {
		spec["scheme"] = "HTTPS"
		spec["tlsConfig"] = prometheusTLSConfig(local)
	}
	return monitoringObject(scrapeConfigGVK, cp.Namespace, "simplyblock-metrics", spec)
}

// prometheusTLSConfig verifies the management API against its CA and, under
// mutual TLS, presents Prometheus's own certificate where it has one and the
// serving certificate on OpenShift (#863).
func prometheusTLSConfig(local *simplyblockv1alpha2.LocalControlPlane) map[string]any {
	secretKey := func(secret, key string) map[string]any {
		return map[string]any{"secret": map[string]any{"name": secret, "key": key}}
	}
	certSecret := ServingCertSecret
	if prometheusHasOwnCertificate(local) {
		certSecret = prometheusClientSecret
	}
	tls := map[string]any{"serverName": ComponentWebAPI, "ca": secretKey(certSecret, "ca.crt")}
	if local.TLSProvider() == simplyblockv1alpha2.ControlPlaneTLSOpenShift {
		tls["ca"] = map[string]any{"configMap": map[string]any{"name": openShiftCAConfigMap, "key": openShiftCAKey}}
	}
	if local.RequiresClientCertificate() {
		tls["cert"] = secretKey(certSecret, corev1.TLSCertKey)
		tls["keySecret"] = map[string]any{"name": certSecret, "key": corev1.TLSPrivateKeyKey}
	}
	return tls
}

// fdbExporterMonitor scrapes the database exporter, keeping the labels it sets.
func fdbExporterMonitor(namespace string) *unstructured.Unstructured {
	return monitoringObject(serviceMonitorGVK, namespace, ComponentFDBExporter, map[string]any{
		"selector": map[string]any{"matchLabels": map[string]any{appLabel: ComponentFDBExporter}},
		"endpoints": []any{map[string]any{
			"port": "metrics", "path": "/metrics", "honorLabels": true,
		}},
	})
}

// rebalancerProbeMonitor scrapes the rebalancer probes, found by the
// annotations the webhook sets on them.
func rebalancerProbeMonitor(namespace string) *unstructured.Unstructured {
	return monitoringObject(podMonitorGVK, namespace, "simplyblock-rebalancer-probe", map[string]any{
		"selector": map[string]any{},
		"podMetricsEndpoints": []any{map[string]any{
			"port": "latency-metrics",
			"relabelings": []any{
				map[string]any{
					"sourceLabels": []any{"__meta_kubernetes_pod_annotation_prometheus_simplyblock_io_scrape"},
					"action":       "keep", "regex": "true",
				},
				map[string]any{
					"sourceLabels": []any{"__meta_kubernetes_pod_annotation_prometheus_simplyblock_io_path"},
					"regex":        "(.+)", "targetLabel": "__metrics_path__",
				},
			},
		}},
	})
}

// prometheusResource is one server with a volume and the Thanos sidecar. It runs
// the image and user the StatefulSet it replaces ran, so a copied volume is
// readable, and carries the label the Service selects.
func prometheusResource(cp *simplyblockv1alpha2.ControlPlane) *unstructured.Unstructured {
	local := cp.Spec.Source.Local
	selector := map[string]any{"matchLabels": map[string]any{scrapedByLabel: prometheusName}}
	claim := map[string]any{
		"accessModes": []any{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]any{"storage": prometheusVolumeSize}},
	}
	if class := storageClassOf(cp); class != "" {
		claim["storageClassName"] = class
	}
	spec := map[string]any{
		"replicas":           int64(1),
		"image":              image(prometheusImage),
		"version":            "v2.51.0",
		"imagePullPolicy":    string(pullPolicyOf(local)),
		"serviceAccountName": prometheusServiceAccount,
		"securityContext": map[string]any{
			"runAsUser": int64(65534), "runAsGroup": int64(65534), "fsGroup": int64(65534), "runAsNonRoot": true,
		},
		"podMetadata": map[string]any{
			"labels":      map[string]any{appLabel: ComponentPrometheus},
			"annotations": map[string]any{utils.AnnotationLogCollector: "true"},
		},
		"scrapeInterval":         "30s",
		"externalLabels":         map[string]any{"monitor": "codelab-monitor"},
		"retention":              prometheusRetention,
		"scrapeConfigSelector":   selector,
		"serviceMonitorSelector": selector,
		"podMonitorSelector":     selector,
		"resources":              resources("100m", "512Mi", "500m", "1Gi"),
		"storage":                map[string]any{"volumeClaimTemplate": map[string]any{"spec": claim}},
		"volumes": []any{map[string]any{
			"name":      prometheusObjstoreVolume,
			"configMap": map[string]any{"name": objectStoreConfigName},
		}},
		"thanos": map[string]any{
			"image":                   image(thanosImage),
			"version":                 "v0.31.0",
			"objectStorageConfigFile": "/etc/thanos/objstore.yml",
			"volumeMounts": []any{map[string]any{
				"name": prometheusObjstoreVolume, "mountPath": "/etc/thanos", "readOnly": true,
			}},
			"resources": resources("100m", "256Mi", "250m", "1Gi"),
		},
	}
	if len(local.NodeSelector) > 0 {
		spec["nodeSelector"] = toUnstructured(local.NodeSelector)
	}
	if len(local.Tolerations) > 0 {
		spec["tolerations"] = toUnstructured(local.Tolerations)
	}
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(prometheusGVK)
	u.SetNamespace(cp.Namespace)
	u.SetName(prometheusName)
	return u
}

func resources(cpu, memory, cpuLimit, memoryLimit string) map[string]any {
	return map[string]any{
		"requests": map[string]any{"cpu": cpu, "memory": memory},
		"limits":   map[string]any{"cpu": cpuLimit, "memory": memoryLimit},
	}
}

// toUnstructured turns a typed value into the plain form an unstructured
// object holds.
func toUnstructured(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

// prometheusService publishes the server and the sidecar's two ports, which is
// what Grafana's Thanos datasource queries.
func prometheusService(namespace string) *corev1.Service {
	labels := map[string]string{appLabel: ComponentPrometheus}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: ComponentPrometheus, Namespace: namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 9090, TargetPort: intstrFromInt(9090)},
				{Name: "grpc", Port: thanosGRPCPort, TargetPort: intstrFromInt(thanosGRPCPort)},
				{Name: "http-thanos", Port: thanosHTTPPort, TargetPort: intstrFromInt(thanosHTTPPort)},
			},
		},
	}
}
