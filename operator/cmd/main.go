/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/controller"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
	internalwebhook "github.com/simplyblock/simplyblock-operator/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const (
	openShiftConfigAPIGroup = "config.openshift.io"
	certManagerAPIGroup     = "cert-manager.io"
)

type serverGroupsGetter interface {
	ServerGroups() (*metav1.APIGroupList, error)
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(simplyblockv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func validateTLSConfiguration(discoveryClient serverGroupsGetter, tlsEnabled bool, tlsProvider string) error {
	if !tlsEnabled {
		return nil
	}
	if !utils.TLSProviderSupported(tlsProvider) {
		return fmt.Errorf("unsupported SB_TLS_PROVIDER %q", tlsProvider)
	}

	requiredGroup := openShiftConfigAPIGroup
	switch utils.NormalizeTLSProvider(tlsProvider) {
	case utils.TLSProviderCertManager:
		requiredGroup = certManagerAPIGroup
	case utils.TLSProviderOpenShift:
		requiredGroup = openShiftConfigAPIGroup
	}

	groupList, err := discoveryClient.ServerGroups()
	if err != nil {
		return fmt.Errorf("discover API groups: %w", err)
	}
	for _, group := range groupList.Groups {
		if group.Name == requiredGroup {
			return nil
		}
	}
	return fmt.Errorf("SB_TLS_SERVE=1 with SB_TLS_PROVIDER=%q requires API group %q", tlsProvider, requiredGroup)
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	var latencyPercentile string
	flag.StringVar(&latencyPercentile, "latency-percentile", "p50",
		"fio write-latency percentile driving the volume-rebalancing deviation signal: "+
			"\"p50\" (median, stable) or \"p99\" (tail, noisy).")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		setupLog.Error(err, "unable to determine operator namespace")
		os.Exit(1)
	}
	operatorNamespace := strings.TrimSpace(string(nsBytes))

	if latencyPercentile != "p50" && latencyPercentile != "p99" {
		setupLog.Error(nil, "invalid --latency-percentile (must be \"p50\" or \"p99\")",
			"value", latencyPercentile)
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
		// Serve from the location the runtime cert provisioner writes to; this is
		// also controller-runtime's default, but we set it explicitly so the value
		// is discoverable alongside the rotator/provisioner in internal/webhook.
		CertDir: utils.WebhookCertDir,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: true,
		TLSOpts:       tlsOpts,
		// FilterProvider protects the metrics endpoint with authn/authz.
		// Only authorized users and service accounts can access metrics.
		// RBAC is configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		FilterProvider: filters.WithAuthenticationAndAuthorization,
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	cfg := ctrl.GetConfigOrDie()
	tlsEnabled := os.Getenv("SB_TLS_SERVE") == "1"
	tlsProvider := utils.NormalizeTLSProvider(os.Getenv("SB_TLS_PROVIDER"))
	tlsMutualEnabled := os.Getenv("SB_TLS_CONNECT") == "authenticated"
	if tlsEnabled {
		discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
		if err != nil {
			setupLog.Error(err, "unable to initialize cluster discovery client for TLS validation")
			os.Exit(1)
		}
		if err := validateTLSConfiguration(discoveryClient, tlsEnabled, tlsProvider); err != nil {
			setupLog.Error(err, "invalid TLS configuration")
			os.Exit(1)
		}
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "283b3903.simplyblock.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.ControlPlaneReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("controlplane-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ControlPlane")
		os.Exit(1)
	}
	if err := (&controller.StorageClusterReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorder("storagecluster-controller"),
		Namespace: operatorNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageCluster")
		os.Exit(1)
	}
	if err := (&controller.StorageNodeSetReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Namespace:        operatorNamespace,
		TLSEnabled:       tlsEnabled,
		TLSProvider:      tlsProvider,
		TLSMutualEnabled: tlsMutualEnabled,
		Recorder:         mgr.GetEventRecorder("storagenodeset-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageNodeSet")
		os.Exit(1)
	}
	if err := (&controller.PoolReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("pool-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Pool")
		os.Exit(1)
	}
	if err := (&controller.TaskReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Task")
		os.Exit(1)
	}
	if err := (&controller.NodeDrainCoordinatorReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		ManagerNodeName:  os.Getenv("NODE_NAME"),
		TLSEnabled:       tlsEnabled,
		TLSMutualEnabled: tlsMutualEnabled,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NodeDrainCoordinator")
		os.Exit(1)
	}
	if err := (&controller.SnapshotReplicationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SnapshotReplication")
		os.Exit(1)
	}
	if err := (&controller.StorageBackupReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("storagebackup-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageBackup")
		os.Exit(1)
	}
	if err := (&controller.BackupRestoreReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("backuprestore-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BackupRestore")
		os.Exit(1)
	}
	if err := (&controller.StorageBackupSyncReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("storagebackupsync-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageBackupSync")
		os.Exit(1)
	}
	if err := (&controller.BackupPolicyReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("backuppolicy-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BackupPolicy")
		os.Exit(1)
	}
	if err := (&controller.BackupImportReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("backupimport-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BackupImport")
		os.Exit(1)
	}
	if err := (&controller.VolumeMigrationReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("volumemigration-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VolumeMigration")
		os.Exit(1)
	}
	if err := (&controller.PersistentVolumeClaimReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("pinnedvolume-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PersistentVolumeClaim")
		os.Exit(1)
	}
	if err := (&controller.StorageNodeReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Recorder:         mgr.GetEventRecorder("storagenode-controller"),
		TLSEnabled:       tlsEnabled,
		TLSMutualEnabled: tlsMutualEnabled,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageNode")
		os.Exit(1)
	}
	if err := (&controller.StorageNodeOpsReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("storagenodeops-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageNodeOps")
		os.Exit(1)
	}
	if err := (&controller.VolumeRebalancerReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Recorder:          mgr.GetEventRecorder("volumerebalancer-controller"),
		LatencyPercentile: latencyPercentile,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VolumeRebalancer")
		os.Exit(1)
	}
	if err := (&controller.StorageNodeLatencyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		// FIXME: test environments use the WebAPI provisioner to explicitly create the
		// benchmark pool/volumes. In production these are auto-provisioned during cluster
		// setup, where the default (nil → AutomaticBenchmarkProvisioner) is correct.
		Provisioner: &controller.WebAPIBenchmarkProvisioner{
			APIClient: webapi.NewClient(),
			K8sClient: mgr.GetClient(),
		},
		// Resolves each storage node's data-network IP (/nics) for the fio baseline;
		// independent of the provisioner, so it is set for both prod and test paths.
		APIClient: webapi.NewClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageNodeLatency")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// Provision the mutating-webhook serving certificate at runtime (self-signed
	// via cert-controller, or from cert-manager when SB_TLS_PROVIDER=cert-manager).
	webhookReady, err := internalwebhook.SetupWebhookCertificate(mgr, operatorNamespace, tlsProvider)
	if err != nil {
		setupLog.Error(err, "unable to set up webhook serving certificate")
		os.Exit(1)
	}

	// Defer registering the webhook until the serving cert is on disk and the CA
	// bundle is injected. This delays adding controller-runtime's webhook-server
	// runnable (whose certwatcher fails on a missing cert file) until the cert
	// exists. failurePolicy=Ignore keeps pod creation unblocked during the gap.
	go func() {
		<-webhookReady
		mgr.GetWebhookServer().Register("/mutate-v1-pod-simplyblock-rebalancer",
			&webhook.Admission{Handler: &internalwebhook.SimplyblockRebalancerInjector{Client: mgr.GetClient()}})
		setupLog.Info("registered simplyblock-rebalancer mutating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha1-storagenode",
			&webhook.Admission{Handler: &internalwebhook.StorageNodeValidator{OperatorNamespace: operatorNamespace}})
		setupLog.Info("registered storagenode validating webhook")

		mgr.GetWebhookServer().Register("/validate-v1-pvc-pinned-volume",
			&webhook.Admission{Handler: &internalwebhook.PersistentVolumeClaimValidator{Client: mgr.GetClient(), APIClient: webapi.NewClient()}})
		setupLog.Info("registered pinned-volume validating webhook")
	}()

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
