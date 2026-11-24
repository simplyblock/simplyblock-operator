// The Prometheus a managed control plane runs, applied with the management API.
// Every control-plane container pushes to it, and its Thanos sidecar uploads
// each two-hour block to the object store the datastore step created.

package controlplane

import (
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	prometheusImage = "quay.io/simplyblock-io/prometheus:v2.51.0@sha256:5ccad477d0057e62a7cd1981ffcc43785ac10c5a35522dc207466ff7e7ec845f"
	thanosImage     = "quay.io/simplyblock-io/thanos:v0.31.0@sha256:e7d337d6ac2aea3f0f9314ec9830291789e16e2b480b9d353be02d05ce7f2a7e"

	// prometheusServiceAccount is the account the management API trusts for
	// scrapes, through SB_K8S_METRICS_SERVICE_ACCOUNTS.
	prometheusServiceAccount = "simplyblock-prometheus"
	prometheusConfigName     = "simplyblock-prometheus-config"

	// prometheusHeadlessService gives each replica the DNS name the chart's
	// Thanos Query dials: simplyblock-prometheus-<i>.<this>.<namespace>.svc.
	prometheusHeadlessService = "simplyblock-prometheus-headless"

	// prometheusDataVolume is the chart's claim template name, so a StatefulSet
	// the chart created is replaced by this one on the same PVC.
	prometheusDataVolume = "storage-volume"
	prometheusVolumeSize = "20Gi"

	prometheusTLSPath = "/etc/prometheus/tls"

	// prometheusClientCertificate and its Secret are the names the chart issued
	// them under, so an install that came from the chart already has the Secret.
	prometheusClientCertificate = "simplyblock-prometheus-client"
	prometheusClientSecret      = "simplyblock-prometheus-client-tls"
	thanosGRPCPort              = 10901
	thanosHTTPPort              = 10902
)

// prometheusObjects is the account and its pod discovery grant, the scrape
// configuration, the server, and its Service.
func prometheusObjects(cp *simplyblockv1alpha2.ControlPlane) []client.Object {
	ns := cp.Namespace
	objects := []client.Object{
		serviceAccount(ns, prometheusServiceAccount, nil),
		prometheusRole(ns),
		prometheusRoleBinding(ns),
		prometheusConfig(cp),
		prometheusStatefulSet(cp),
		prometheusService(ns),
		prometheusHeadless(ns),
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

// prometheusTLSVolume is Prometheus's own client Secret where it has one, which
// carries the CA it verifies the management API against, and the shared
// control-plane volume otherwise.
func prometheusTLSVolume(local *simplyblockv1alpha2.LocalControlPlane) []corev1.Volume {
	if !prometheusHasOwnCertificate(local) {
		return tlsVolume(local)
	}
	return []corev1.Volume{{
		Name: tlsVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: prometheusClientSecret},
		},
	}}
}

// prometheusRole lets the server discover the rebalancer probe pods in its own
// namespace and nothing else.
func prometheusRole(namespace string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: prometheusServiceAccount, Namespace: namespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get", "list", "watch"},
		}},
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

// prometheusConfig is the chart's scrape configuration: the management API,
// authenticated with the account's token, the database exporter, and the
// rebalancer probes found by their pod annotations.
func prometheusConfig(cp *simplyblockv1alpha2.ControlPlane) *corev1.ConfigMap {
	local := cp.Spec.Source.Local
	var b strings.Builder
	b.WriteString(`global:
  scrape_interval: 30s
  external_labels:
    monitor: 'codelab-monitor'

scrape_configs:
  - job_name: 'simplyblock_metrics'
    scrape_timeout: 30s
    static_configs:
      - targets: ['` + ComponentWebAPI + `:5000']
    metrics_path: '/api/v2/metrics'
`)
	if local.ServesTLS() {
		b.WriteString(`    scheme: https
    tls_config:
      ca_file: ` + prometheusTLSPath + `/ca.crt
      server_name: ` + ComponentWebAPI + `
`)
		if local.RequiresClientCertificate() {
			b.WriteString(`      cert_file: ` + prometheusTLSPath + `/tls.crt
      key_file: ` + prometheusTLSPath + `/tls.key
`)
		}
	}
	b.WriteString(`    authorization:
      credentials_file: /var/run/secrets/kubernetes.io/serviceaccount/token

  - job_name: 'fdb_metrics'
    static_configs:
      - targets: ['` + ComponentFDBExporter + `:9444']
        labels:
          namespace: ` + cp.Namespace + `
    honor_labels: true
    metrics_path: '/metrics'

  - job_name: 'simplyblock_rebalancer_probe'
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names: ['` + cp.Namespace + `']
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_simplyblock_io_scrape]
        action: keep
        regex: 'true'
      - source_labels: [__meta_kubernetes_pod_container_port_name]
        action: keep
        regex: latency-metrics
      - source_labels: [__meta_kubernetes_pod_ip, __meta_kubernetes_pod_annotation_prometheus_simplyblock_io_port]
        action: replace
        regex: '(.+);(.+)'
        replacement: '$1:$2'
        target_label: __address__
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_simplyblock_io_path]
        action: replace
        regex: '(.+)'
        target_label: __metrics_path__
`)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: prometheusConfigName, Namespace: cp.Namespace,
			Labels: map[string]string{appLabel: ComponentPrometheus},
		},
		Data: map[string]string{"prometheus.yml": b.String()},
	}
}

// prometheusStatefulSet is one server with a volume, and the Thanos sidecar
// reading the same volume. The block durations are fixed at two hours so the
// sidecar uploads every block rather than waiting on a compaction.
func prometheusStatefulSet(cp *simplyblockv1alpha2.ControlPlane) *appsv1.StatefulSet {
	local := cp.Spec.Source.Local
	labels := map[string]string{appLabel: ComponentPrometheus}
	const configVolume, objstoreVolume = "config", "objstore-config"

	prometheusMounts := []corev1.VolumeMount{
		{Name: prometheusDataVolume, MountPath: "/prometheus"},
		{Name: configVolume, MountPath: "/etc/prometheus/config", ReadOnly: true},
	}
	volumes := []corev1.Volume{
		{Name: configVolume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: prometheusConfigName},
		}}},
		{Name: objstoreVolume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: objectStoreConfigName},
		}}},
	}
	if local.ServesTLS() {
		volumes = append(volumes, prometheusTLSVolume(local)...)
		prometheusMounts = append(prometheusMounts,
			corev1.VolumeMount{Name: tlsVolumeName, MountPath: prometheusTLSPath, ReadOnly: true})
	}

	spec := corev1.PodSpec{
		ServiceAccountName: prometheusServiceAccount,
		// The image runs as nobody; the group makes a fresh volume writable.
		SecurityContext: &corev1.PodSecurityContext{FSGroup: ptr.To(int64(65534))},
		Volumes:         volumes,
		Containers: []corev1.Container{
			{
				Name:            "prometheus",
				Image:           image(prometheusImage),
				ImagePullPolicy: pullPolicyOf(local),
				Args: []string{
					"--config.file=/etc/prometheus/config/prometheus.yml",
					"--storage.tsdb.path=/prometheus",
					"--storage.tsdb.min-block-duration=2h",
					"--storage.tsdb.max-block-duration=2h",
					"--web.enable-lifecycle",
				},
				Ports:        []corev1.ContainerPort{{Name: "web", ContainerPort: 9090}},
				VolumeMounts: prometheusMounts,
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
						Path: "/-/ready", Port: intstrFromInt(9090),
					}},
					InitialDelaySeconds: 10,
					PeriodSeconds:       10,
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			},
			{
				Name:            "thanos-sidecar",
				Image:           image(thanosImage),
				ImagePullPolicy: pullPolicyOf(local),
				Args: []string{
					"sidecar",
					"--tsdb.path=/prometheus",
					"--prometheus.url=http://localhost:9090",
					"--objstore.config-file=/etc/thanos/objstore.yml",
				},
				Ports: []corev1.ContainerPort{
					{Name: "grpc", ContainerPort: thanosGRPCPort},
					{Name: "http", ContainerPort: thanosHTTPPort},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: prometheusDataVolume, MountPath: "/prometheus"},
					{Name: objstoreVolume, MountPath: "/etc/thanos", ReadOnly: true},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("250m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			},
		},
	}
	scheduling(local, &spec)

	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: prometheusDataVolume},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(prometheusVolumeSize),
				},
			},
		},
	}
	// The same class as the database and the object store, for the same reason.
	if class := storageClassOf(cp); class != "" {
		claim.Spec.StorageClassName = ptr.To(class)
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: ComponentPrometheus, Namespace: cp.Namespace, Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: prometheusHeadlessService,
			Replicas:    ptr.To(int32(1)),
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{utils.AnnotationLogCollector: "true"},
				},
				Spec: spec,
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{claim},
		},
	}
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

// prometheusHeadless governs the StatefulSet, so each replica's sidecar has a
// stable address for Thanos Query.
func prometheusHeadless(namespace string) *corev1.Service {
	labels := map[string]string{appLabel: ComponentPrometheus}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: prometheusHeadlessService, Namespace: namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  labels,
			Ports: []corev1.ServicePort{
				{Name: "grpc", Port: thanosGRPCPort, TargetPort: intstrFromInt(thanosGRPCPort)},
			},
		},
	}
}
