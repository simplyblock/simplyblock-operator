// The Prometheus scrape targets for every storage node's SPDK proxy.
//
// The proxy answers Prometheus metrics on the same port and under the same
// Basic-auth credential as its RPCs (sbcli's spdk_http_proxy_server.py,
// require_authorization on /_meta/metrics), and that credential is generated
// per node rather than shared across the cluster. A single static_configs job
// with one basic_auth block cannot authenticate against more than one node, so
// this writes one scrape_config per node instead, each carrying that node's
// own credential.
//
// The ConfigMap this writes is a second, operator-owned file alongside the
// human/Helm-owned simplyblock-prometheus-config: Prometheus's own
// scrape_config_files include stitches the two together, so nothing here has
// to parse or merge into the other's single prometheus.yml blob.

package node

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// SpdkProxyScrapeConfigMapName is the ConfigMap the chart mounts into
// Prometheus's scrape_config_files, alongside simplyblock-prometheus-config.
const SpdkProxyScrapeConfigMapName = "simplyblock-spdk-proxy-scrape-config"

// spdkProxyScrapeConfigKey is the one file the ConfigMap carries. The chart
// mounts the whole ConfigMap as a directory and Prometheus globs every file in
// it, so one key covers every node this reconciles.
const spdkProxyScrapeConfigKey = "spdk-proxy.yml"

const spdkProxyMetricsPath = "/_meta/metrics"

// scrapeConfig is the subset of Prometheus's <scrape_config> this writes. It
// marshals through sigs.k8s.io/yaml, so field names come from the JSON tags.
type scrapeConfig struct {
	JobName       string         `json:"job_name"`
	MetricsPath   string         `json:"metrics_path"`
	Scheme        string         `json:"scheme,omitempty"`
	StaticConfigs []staticConfig `json:"static_configs"`
	BasicAuth     basicAuth      `json:"basic_auth"`
	TLSConfig     *tlsConfig     `json:"tls_config,omitempty"`
}

type staticConfig struct {
	Targets []string `json:"targets"`
}

type basicAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type tlsConfig struct {
	CAFile     string `json:"ca_file"`
	ServerName string `json:"server_name"`
	CertFile   string `json:"cert_file,omitempty"`
	KeyFile    string `json:"key_file,omitempty"`
}

// reconcileSpdkProxyScrapeConfig writes the SPDK proxy's Prometheus scrape
// targets, one scrape_config per node, each carrying that node's own
// RPC_USERNAME/RPC_PASSWORD, the same credential the proxy already requires
// for its RPC traffic.
//
// It shares reconcileSpdkProxyEndpoints's pod list and its port/readiness
// tests rather than listing again, so a pod this publishes a scrape target
// for is exactly a pod the other publishes a DNS name for, and the target
// address is that same DNS name: <node>.simplyblock-spdk-proxy.<ns>.svc.
func (r *StorageNodeWorkloadReconciler) reconcileSpdkProxyScrapeConfig(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	log := logf.FromContext(ctx)

	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{"role": utils.LabelSpdkProxyRole},
	); err != nil {
		return fmt.Errorf("list the spdk-proxy pods: %w", err)
	}

	configs := make([]scrapeConfig, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !spdkProxyPodServesAnAddress(pod) {
			continue
		}
		rpcPort, ok := spdkProxyRPCPort(pod)
		if !ok {
			continue
		}
		username, password, ok := spdkProxyRPCCredentials(pod)
		if !ok {
			log.Info("an spdk-proxy pod names no RPC credentials and is not scraped",
				"pod", pod.Name)
			continue
		}

		nodeLabel := utils.NodeHostnameLabel(pod.Spec.NodeName)
		host := fmt.Sprintf("%s.%s.%s.svc.cluster.local", nodeLabel, spdkProxyServiceName, cluster.Namespace)

		config := scrapeConfig{
			JobName:       "spdk_proxy_" + nodeLabel,
			MetricsPath:   spdkProxyMetricsPath,
			StaticConfigs: []staticConfig{{Targets: []string{fmt.Sprintf("%s:%d", host, rpcPort)}}},
			BasicAuth:     basicAuth{Username: username, Password: password},
		}
		if r.TLSEnabled {
			config.Scheme = "https"
			config.TLSConfig = &tlsConfig{
				CAFile:     "/etc/prometheus/ca/ca.crt",
				ServerName: host,
			}
			if r.TLSMutualEnabled {
				config.TLSConfig.CertFile = "/etc/prometheus/certs/tls.crt"
				config.TLSConfig.KeyFile = "/etc/prometheus/certs/tls.key"
			}
		}
		configs = append(configs, config)
	}
	// Stable output is what makes applySpdkProxyScrapeConfigMap's equality check
	// skip a write when nothing changed, since map iteration order over the pod
	// list is not stable on its own.
	sort.Slice(configs, func(i, j int) bool { return configs[i].JobName < configs[j].JobName })

	rendered, err := yaml.Marshal(configs)
	if err != nil {
		return fmt.Errorf("render the spdk-proxy scrape config: %w", err)
	}

	return r.applySpdkProxyScrapeConfigMap(ctx, cluster, rendered)
}

// spdkProxyRPCCredentials reads the Basic-auth pair the proxy's RPC port and
// its /_meta/metrics endpoint are both locked behind. The control plane sets
// it per node as plain container env vars rather than a mounted Secret, so
// reading it back here needs no RBAC beyond the pod read reconcileSpdkProxyEndpoints
// already does.
func spdkProxyRPCCredentials(pod *corev1.Pod) (username, password string, ok bool) {
	for _, container := range pod.Spec.Containers {
		if container.Name != "spdk-proxy-container" {
			continue
		}
		for _, env := range container.Env {
			switch env.Name {
			case "RPC_USERNAME":
				username = env.Value
			case "RPC_PASSWORD":
				password = env.Value
			}
		}
		return username, password, username != "" && password != ""
	}
	return "", "", false
}

// applySpdkProxyScrapeConfigMap creates or updates the shared ConfigMap, owned
// by this cluster the same way the shared spdk-proxy Service already is
// (reconcileService): in the common, single-StorageCluster-per-namespace
// deployment this is the only writer, and a second cluster in the same
// namespace converges to the same content from the same pod list, so which of
// the two owns the object does not change what it contains.
func (r *StorageNodeWorkloadReconciler) applySpdkProxyScrapeConfigMap(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, rendered []byte,
) error {
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SpdkProxyScrapeConfigMapName,
			Namespace: cluster.Namespace,
		},
		Data: map[string]string{
			spdkProxyScrapeConfigKey: string(rendered),
		},
	}
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return fmt.Errorf("own the spdk-proxy scrape config: %w", err)
	}

	var existing corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("read the spdk-proxy scrape config: %w", err)
	}
	if existing.Data[spdkProxyScrapeConfigKey] == desired.Data[spdkProxyScrapeConfigKey] {
		return nil
	}
	desired.ResourceVersion = existing.ResourceVersion
	return r.Update(ctx, desired)
}
