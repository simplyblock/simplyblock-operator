// The Prometheus scrape targets this reconciles for every SPDK proxy.

package node

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// withRPCCredentials adds the Basic-auth pair the control plane sets per node,
// which anSPDKPod leaves out because reconcileSpdkProxyEndpoints never reads it.
func withRPCCredentials(pod *corev1.Pod, username, password string) *corev1.Pod {
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "RPC_USERNAME", Value: username},
		corev1.EnvVar{Name: "RPC_PASSWORD", Value: password},
	)
	return pod
}

func scrapeConfigMap(t *testing.T, r *StorageNodeWorkloadReconciler) *corev1.ConfigMap {
	t.Helper()
	var configMap corev1.ConfigMap
	key := client.ObjectKey{Namespace: "simplyblock", Name: SpdkProxyScrapeConfigMapName}
	if err := r.Get(context.Background(), key, &configMap); err != nil {
		t.Fatalf("the spdk-proxy scrape config was not published: %v", err)
	}
	return &configMap
}

func decodedScrapeConfigs(t *testing.T, configMap *corev1.ConfigMap) []scrapeConfig {
	t.Helper()
	var configs []scrapeConfig
	if err := yaml.Unmarshal([]byte(configMap.Data[spdkProxyScrapeConfigKey]), &configs); err != nil {
		t.Fatalf("the published scrape config is not valid YAML: %v", err)
	}
	return configs
}

// A node's proxy carries its own credential, so its scrape target does too:
// one job per node, addressed at the same per-pod DNS name the control plane
// itself resolves, authenticated with that node's own Basic-auth pair.
func TestTheScrapeConfigCarriesEachNodesOwnCredential(t *testing.T) {
	r, cluster := aProxyReconciler(t,
		withRPCCredentials(
			anSPDKPod("snode-spdk-pod-4420-abc", "worker-0.ocp.simplyblock.ai", "10.0.0.10", 4420),
			"node-0", "secret-0"),
	)

	if err := r.reconcileSpdkProxyScrapeConfig(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile the scrape config: %v", err)
	}

	configs := decodedScrapeConfigs(t, scrapeConfigMap(t, r))
	if len(configs) != 1 {
		t.Fatalf("published %d scrape_configs, want 1", len(configs))
	}
	config := configs[0]
	if config.MetricsPath != "/_meta/metrics" {
		t.Errorf("metrics_path is %q", config.MetricsPath)
	}
	wantTarget := "worker-0.simplyblock-spdk-proxy.simplyblock.svc.cluster.local:4420"
	if len(config.StaticConfigs) != 1 || len(config.StaticConfigs[0].Targets) != 1 ||
		config.StaticConfigs[0].Targets[0] != wantTarget {
		t.Errorf("the target is %v, want [%s]", config.StaticConfigs, wantTarget)
	}
	if config.BasicAuth.Username != "node-0" || config.BasicAuth.Password != "secret-0" {
		t.Errorf("the job's basic_auth is %+v, want this node's own credential", config.BasicAuth)
	}
	if config.TLSConfig != nil {
		t.Errorf("tls_config is set with TLS disabled: %+v", config.TLSConfig)
	}
}

// Two nodes cannot share one job, because basic_auth is a property of the job
// and each node's proxy is locked behind its own.
func TestEachNodeGetsItsOwnJob(t *testing.T) {
	r, cluster := aProxyReconciler(t,
		withRPCCredentials(
			anSPDKPod("snode-spdk-pod-4420-abc", "worker-0.ocp.simplyblock.ai", "10.0.0.10", 4420),
			"node-0", "secret-0"),
		withRPCCredentials(
			anSPDKPod("snode-spdk-pod-4422-abc", "worker-1.ocp.simplyblock.ai", "10.0.0.11", 4422),
			"node-1", "secret-1"),
	)

	if err := r.reconcileSpdkProxyScrapeConfig(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile the scrape config: %v", err)
	}

	configs := decodedScrapeConfigs(t, scrapeConfigMap(t, r))
	if len(configs) != 2 {
		t.Fatalf("published %d scrape_configs, want 2", len(configs))
	}
	if configs[0].JobName == configs[1].JobName {
		t.Errorf("both jobs are named %q", configs[0].JobName)
	}
}

// A pod the control plane has not yet set a credential on is left out rather
// than published with an empty basic_auth, which would scrape unauthenticated
// and be refused by the proxy's own require_authorization.
func TestAPodWithNoCredentialsIsNotScraped(t *testing.T) {
	r, cluster := aProxyReconciler(t,
		anSPDKPod("snode-spdk-pod-4420-abc", "worker-0.ocp.simplyblock.ai", "10.0.0.10", 4420),
	)

	if err := r.reconcileSpdkProxyScrapeConfig(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile the scrape config: %v", err)
	}

	err := r.Get(context.Background(), client.ObjectKey{
		Namespace: "simplyblock", Name: SpdkProxyScrapeConfigMapName,
	}, &corev1.ConfigMap{})
	if err == nil {
		configs := decodedScrapeConfigs(t, scrapeConfigMap(t, r))
		if len(configs) != 0 {
			t.Errorf("a pod with no credentials was published as %+v", configs)
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("read the scrape config: %v", err)
	}
}

// TLS on means every job scrapes over HTTPS, verified against the cluster's CA
// and SNI'd to the same per-pod hostname the serving certificate's wildcard SAN
// covers (*.simplyblock-spdk-proxy.<ns>.svc.cluster.local).
func TestTheScrapeConfigUsesTLSWhenEnabled(t *testing.T) {
	r, cluster := aProxyReconciler(t,
		withRPCCredentials(
			anSPDKPod("snode-spdk-pod-4420-abc", "worker-0.ocp.simplyblock.ai", "10.0.0.10", 4420),
			"node-0", "secret-0"),
	)
	r.TLSEnabled = true

	if err := r.reconcileSpdkProxyScrapeConfig(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile the scrape config: %v", err)
	}

	configs := decodedScrapeConfigs(t, scrapeConfigMap(t, r))
	if len(configs) != 1 {
		t.Fatalf("published %d scrape_configs, want 1", len(configs))
	}
	config := configs[0]
	if config.Scheme != "https" {
		t.Errorf("scheme is %q with TLS enabled", config.Scheme)
	}
	wantServerName := "worker-0.simplyblock-spdk-proxy.simplyblock.svc.cluster.local"
	if config.TLSConfig == nil || config.TLSConfig.ServerName != wantServerName {
		t.Errorf("tls_config is %+v, want server_name %q", config.TLSConfig, wantServerName)
	}
	if config.TLSConfig != nil && config.TLSConfig.CertFile != "" {
		t.Errorf("cert_file is set with mutual TLS disabled: %+v", config.TLSConfig)
	}
}

// A second pass over the same pods writes nothing: the equality check in
// applySpdkProxyScrapeConfigMap is what keeps an unchanged node set from
// rolling Prometheus's StatefulSet every reconcile (the chart's
// reloader.stakater.com annotation restarts it on any ConfigMap write).
func TestAnUnchangedPassWritesNothing(t *testing.T) {
	r, cluster := aProxyReconciler(t,
		withRPCCredentials(
			anSPDKPod("snode-spdk-pod-4420-abc", "worker-0.ocp.simplyblock.ai", "10.0.0.10", 4420),
			"node-0", "secret-0"),
	)

	if err := r.reconcileSpdkProxyScrapeConfig(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile the scrape config: %v", err)
	}
	before := scrapeConfigMap(t, r).ResourceVersion

	if err := r.reconcileSpdkProxyScrapeConfig(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile the scrape config again: %v", err)
	}
	after := scrapeConfigMap(t, r).ResourceVersion

	if before != after {
		t.Errorf("an unchanged pod set still rewrote the ConfigMap: %s -> %s", before, after)
	}
}

// TestThePassWritesTheScrapeConfig asserts the step is registered in the
// workload pass, the same gap a prior regression left the EndpointSlice
// builder sitting in (see TestThePassPublishesTheProxyEndpoints): a builder
// that is never called passes its own tests while publishing nothing.
func TestThePassWritesTheScrapeConfig(t *testing.T) {
	r, _ := aProxyReconciler(t)

	steps := r.workloadSteps(nil)
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.what)
	}
	if !slices.Contains(names, "the spdk-proxy scrape config") {
		t.Error("no step of the workload pass writes the spdk-proxy scrape config, " +
			"so Prometheus never learns the storage nodes' targets")
	}
}
