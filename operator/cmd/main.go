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
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/placement/placementrpc"
	atlasprom "github.com/simplyblock/atlas/prometheus"
	atlasswitchover "github.com/simplyblock/atlas/switchover"

	"github.com/simplyblock/simplyblock-operator/internal/autoplacement"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/metricsapi"
	"github.com/simplyblock/simplyblock-operator/internal/sitelink"
	"github.com/simplyblock/simplyblock-operator/internal/sitesvc"

	// Import all Kubernetes client auth plugins (e.g., Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	volumegroupsnapshotv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumegroupsnapshot/v1beta1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	workv1 "open-cluster-management.io/api/work/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controller"
	backupcontrollers "github.com/simplyblock/simplyblock-operator/internal/controllers/backup"
	clustercontroller "github.com/simplyblock/simplyblock-operator/internal/controllers/cluster"
	consistencygroupcontrollers "github.com/simplyblock/simplyblock-operator/internal/controllers/consistencygroup"
	controlplanecontroller "github.com/simplyblock/simplyblock-operator/internal/controllers/controlplane"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/deployment"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
	nodecontroller "github.com/simplyblock/simplyblock-operator/internal/controllers/node"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/pool"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/twonode"
	twonodecp "github.com/simplyblock/simplyblock-operator/internal/controllers/twonode/cpadapter"
	volumecontrollers "github.com/simplyblock/simplyblock-operator/internal/controllers/volume"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/vrgfollow"
	"github.com/simplyblock/simplyblock-operator/internal/csilink"
	"github.com/simplyblock/simplyblock-operator/internal/placement"
	"github.com/simplyblock/simplyblock-operator/internal/switchover"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
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
	// ocmWorkGroupVersion is OCM's work API group/version, and ocmManifestWorkResource
	// the ManifestWork resource within it. ManifestWork is served only on the hub;
	// a managed cluster serves the SAME group/version for AppliedManifestWork (the
	// work-agent's local record) but NOT ManifestWork, so the presence check must
	// be resource-level, not group-level — a group-level check sees the group as
	// served everywhere AppliedManifestWork exists. The TestFailover controller
	// watches ManifestWork, so it is registered only where ManifestWork is served.
	ocmWorkGroupVersion     = "work.open-cluster-management.io/v1"
	ocmManifestWorkResource = "manifestworks"
)

type serverGroupsGetter interface {
	ServerGroups() (*metav1.APIGroupList, error)
}

type serverResourcesGetter interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// serverHasResource reports whether the API server serves the named resource in
// the given group/version. Used to skip a controller that watches a kind the
// cluster does not serve, since a watch whose cache can never sync takes the
// whole manager down at startup. A group/version the server does not serve at
// all is reported as absent rather than an error.
func serverHasResource(discoveryClient serverResourcesGetter, groupVersion, resource string) (bool, error) {
	list, err := discoveryClient.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("discover resources for %s: %w", groupVersion, err)
	}
	for _, r := range list.APIResources {
		if r.Name == resource {
			return true, nil
		}
	}
	return false, nil
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	// The cert-manager webhook provisioner injects its CA bundle into the
	// converting kinds' CRDs (internal/webhook/conversion_trust.go), so this
	// process has to know that kind too — the same registration
	// cmd/conversion-webhook/main.go already carries for the same reason.
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(simplyblockv1alpha1.AddToScheme(scheme))
	// v1alpha2 is the shape every controller reads for the kinds that have one.
	// v1alpha1 stays registered because it is still the storage version and still
	// served (design-property-renames.md §3.8).
	utilruntime.Must(simplyblockv1alpha2.AddToScheme(scheme))
	// external-snapshotter VolumeGroupSnapshot: the operator serves a validating
	// webhook on it (design §9.4) but does not own the CRD.
	utilruntime.Must(volumegroupsnapshotv1beta1.AddToScheme(scheme))
	// external-snapshotter VolumeSnapshot: the VolumeGroupSnapshotOps restore
	// enumerates a group snapshot's member snapshots (design §7.4).
	utilruntime.Must(snapshotv1.AddToScheme(scheme))
	// OCM ManifestWork: the TestFailover controller places the bubble PV/PVC on a
	// recovery cluster through it (design-test-failover.md §7.6).
	utilruntime.Must(workv1.Install(scheme))
	// +kubebuilder:scaffold:scheme
}

func validateTLSConfiguration(discoveryClient serverGroupsGetter, providerSet bool, tlsProvider string) error {
	if !providerSet {
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
	return fmt.Errorf("SB_TLS_PROVIDER=%q requires API group %q", tlsProvider, requiredGroup)
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
	var legacyVolumeMigration bool
	flag.BoolVar(&legacyVolumeMigration, "legacy-volume-migration", false,
		"Move volumes as the registered VolumeMigration kind rather than as "+
			"PersistentVolumeOps. Off by default: the redesigned kind is what this API group "+
			"documents, and the registered one is kept for one release so that an upgrade can "+
			"turn it back on while migrations raised against it drain. A rename and a scope "+
			"change make a new CRD rather than a new version, so an in-flight migration cannot "+
			"be carried across.")
	var csiLinkEnabled bool
	var csiLinkAddr, csiLinkCertPath, csiLinkCertName, csiLinkCertKey, csiLinkAudience string
	var csiLinkClientCAFile string
	flag.BoolVar(&csiLinkEnabled, "csi-link", false, "Serve the CSI link.")
	flag.StringVar(&csiLinkAddr, "csi-link-bind-address", ":9500",
		"The address the CSI link endpoint binds to.")
	flag.StringVar(&csiLinkCertPath, "csi-link-cert-path", "",
		"The directory that contains the CSI link serving certificate. Set, the link serves TLS "+
			"only, waiting for the certificate if it is not there yet. Unset serves the link "+
			"without TLS, and peers' bearer tokens then travel in the clear.")
	flag.StringVar(&csiLinkCertName, "csi-link-cert-name", "tls.crt",
		"The name of the CSI link serving certificate file.")
	flag.StringVar(&csiLinkCertKey, "csi-link-cert-key", "tls.key",
		"The name of the CSI link serving key file.")
	flag.StringVar(&csiLinkAudience, "csi-link-audience", "simplyblock-csi-link",
		"The audience the peers' projected ServiceAccount tokens must carry.")
	var siteLinkEnabled bool
	var siteLinkAddr, siteLinkNamespace, siteLinkTokenPath string
	flag.BoolVar(&siteLinkEnabled, "site-link", false,
		"Link to this site's fleet agent, the site end of a stretched cluster's site link.")
	flag.StringVar(&siteLinkAddr, "site-link-address",
		"simplyblock-site-link.open-cluster-management-agent-addon.svc:9443",
		"The site's fleet agent, host:port.")
	flag.StringVar(&siteLinkNamespace, "site-link-namespace", "open-cluster-management-agent-addon",
		"The fleet agent's namespace, where it publishes the CA it serves with.")
	flag.StringVar(&siteLinkTokenPath, "site-link-token-path", "/var/run/secrets/simplyblock/site-link/token",
		"The projected ServiceAccount token, with the site link's audience, the agent verifies.")
	flag.StringVar(&csiLinkClientCAFile, "csi-link-client-ca-file", "",
		"The CA bundle that verifies the CSI peers' client certificates. Set, the link requires "+
			"mutual TLS, admitting only the CSI node and controller client certificates.")

	var enableMetricsAPI bool
	var metricsAPIPort int
	flag.BoolVar(&enableMetricsAPI, "enable-metrics-api", true,
		"Serve the aggregated metrics.simplyblock.io API (LogicalVolumeMetrics and "+
			"StorageDeviceMetrics). Disable it on a cluster where the APIService is "+
			"not installed.")
	flag.IntVar(&metricsAPIPort, "metrics-api-bind-port", metricsapi.DefaultBindPort,
		"The HTTPS port the aggregated metrics API listens on.")
	var prometheusURL string
	flag.StringVar(&prometheusURL, "prometheus-url", utils.DefaultPrometheusURL,
		"Prometheus endpoint the aggregated metrics API reads capacity samples from. "+
			"Empty serves each volume's provisioned size with no measured values, and no "+
			"device readings at all.")
	var latencyPercentile string
	flag.StringVar(&latencyPercentile, "latency-percentile", "p50",
		"fio write-latency percentile driving the volume-rebalancing deviation signal: "+
			"\"p50\" (median, stable) or \"p99\" (tail, noisy).")
	var enableDeviceHealth bool
	var deviceHealthInterval, deviceHealthWindow, deviceSMARTInterval time.Duration
	flag.BoolVar(&enableDeviceHealth, "enable-device-health", true,
		"Assess every storage device's performance against its fleet and read its SMART health log, "+
			"publish the verdicts on the StorageDevice, and report them to the control plane's alerts "+
			"endpoint as device_slow and device_unhealthy.")
	flag.DurationVar(&deviceHealthInterval, "device-health-interval", nodecontroller.DefaultDeviceHealthInterval,
		"How often the storage devices are assessed against their fleet.")
	flag.DurationVar(&deviceHealthWindow, "device-health-window", nodecontroller.DefaultDeviceHealthWindow,
		"The window each performance assessment averages device IO over.")
	flag.DurationVar(&deviceSMARTInterval, "device-smart-interval", nodecontroller.DefaultSMARTInterval,
		"How often each storage device's SMART health log is read. A StorageDeviceOps with action "+
			"CheckHealth reads one immediately.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	clusterDNSDomain, err := utils.DetectClusterDNSDomain()
	if err != nil {
		setupLog.Info("falling back to the default cluster DNS domain",
			"domain", clusterDNSDomain, "reason", err.Error())
	} else {
		setupLog.Info("detected the cluster DNS domain", "domain", clusterDNSDomain)
	}

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
	// SB_TLS_PROVIDER selects where the admission webhooks' serving certificate
	// comes from, and nothing else. The installation's own TLS is stated by the
	// ControlPlane and read from it, because the webhook certificate has to exist
	// before the manager starts and a ControlPlane cannot be read until it has.
	webhookProviderSet := os.Getenv("SB_TLS_PROVIDER") != ""
	tlsProvider := utils.NormalizeTLSProvider(os.Getenv("SB_TLS_PROVIDER"))
	if webhookProviderSet {
		discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
		if err != nil {
			setupLog.Error(err, "unable to initialize cluster discovery client for TLS validation")
			os.Exit(1)
		}
		if err := validateTLSConfiguration(discoveryClient, webhookProviderSet, tlsProvider); err != nil {
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

	// SwitchoverService (design-stretched-sync-replication-ramen.md §9.3): the
	// operator moves a sync volume's leadership to the site a pod stages on,
	// for its own node plugins over the CSI link and for the other site's
	// operator over the site link. It reaches the control plane the
	// ControlPlane object names, with the credential it names.
	switchoverServer := newSwitchoverServer(mgr, operatorNamespace)

	// The CSI link. csiPeers is the registry of linked node and controller
	// plugins; a reconciler reaching a node goes through it, and treats
	// link.ErrNoSession as a requeue rather than a failure.
	//
	// Off by default, on with --csi-link. TLS when a certificate is
	// configured, plaintext when none is, and mutual TLS when a client CA
	// bundle is configured as well.
	var csiPeers *link.Registry
	// Placement for superclusters is served on the link (design-superclusters.md
	// §7.2): the CSI controller asks which member a new volume goes to. Its
	// control-plane lookups and capacity source are set below, beside the
	// supercluster reconciler that feeds it readings.
	placementService := &placement.Service{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorder("placement"),
	}
	placementServer, err := placementrpc.NewServer(placementService)
	if err != nil {
		setupLog.Error(err, "unable to build the placement service")
		os.Exit(1)
	}
	if csiLinkEnabled {
		var certFile, keyFile string
		if csiLinkCertPath != "" {
			certFile = filepath.Join(csiLinkCertPath, csiLinkCertName)
			keyFile = filepath.Join(csiLinkCertPath, csiLinkCertKey)
		}
		var err error
		// ClientNames are the client certificates the chart issues to the two
		// plugins. The metadata server presents the node plugin's.
		csiPeers, err = csilink.Setup(mgr, csilink.Config{
			BindAddress:              csiLinkAddr,
			CertFile:                 certFile,
			KeyFile:                  keyFile,
			ClientCAFile:             csiLinkClientCAFile,
			ClientNames:              []string{"simplyblock-csi-node", "simplyblock-csi-controller"},
			Namespace:                operatorNamespace,
			Audiences:                []string{csiLinkAudience},
			NodeServiceAccount:       "simplyblock-csi-node-sa",
			ControllerServiceAccount: "simplyblock-csi-controller-sa",
			MDSServiceAccount:        "simplyblock-csi-mds-sa",
			Register:                 placementServer.Register,
			Capabilities:             placementrpc.Capabilities(),
		})
		if err != nil {
			setupLog.Error(err, "unable to set up the CSI link")
			os.Exit(1)
		}
	}

	// Control-plane SSE push subscriptions: one leader-only manager, streams
	// driven by scopes that reconcilers register (the StorageNode controller adds
	// a node's scope once the node is provisioned). Devices are mirrored into
	// StorageDevice CRs via a subscription (ingest→cache) + a reconciler
	// (cache→CR writes).
	streamCfg, err := subscriptions.ResolveStreamConfig()
	if err != nil {
		setupLog.Error(err, "unable to resolve control-plane stream config")
		os.Exit(1)
	}
	// What TLS the installation uses is read from the ControlPlane on every use.
	// Nothing is decided at startup: the operator is installed before its
	// ControlPlane exists, and the control plane is reached over whichever scheme
	// that object states once it does.
	controlPlaneTLS := controlplanecontroller.NewTLSResolver(mgr.GetClient(), operatorNamespace)
	webapi.SetTLSPolicy(func(ctx context.Context) bool { return controlPlaneTLS(ctx).Enabled })

	// LeaderOnly: these subscriptions feed reconcilers that write StorageDevice
	// and StorageNode objects, and two replicas writing the same object would
	// fight over it.
	// The storage-plane side of a node, shared by the three reconcilers that touch
	// it. The EndpointSlice a migration blocks on is read straight from the API
	// server: a stale informer cache can miss a freshly published endpoint, and a
	// migration would then wait on DNS forever while the name has in fact resolved
	// for minutes (design-storagenode.md §5.4).
	storageNodeWorkload := &nodecontroller.Workload{
		Client:   mgr.GetClient(),
		Uncached: mgr.GetAPIReader(),
		TLS:      controlPlaneTLS,
		// Which node the manager itself runs on, which the chart sets from
		// spec.nodeName. It decides one question: whether a maintenance window
		// is draining the manager's own host, in which case the manager holds
		// its own eviction while it arranges the window. Empty where nothing
		// set it, and then it holds nothing.
		ManagerNode: os.Getenv("NODE_NAME"),
	}

	// A self-budget outlives a manager that crashed holding one, and would then
	// make its worker undrainable by an object whose owner no longer exists.
	// Clearing it is leader-only, because only the leader ever creates one: a
	// replica clearing it on the way up would be clearing the leader's.
	if err := mgr.Add(leaderOnly(func(ctx context.Context) error {
		return storageNodeWorkload.ClearStaleSelfBudget(ctx, operatorNamespace)
	})); err != nil {
		setupLog.Error(err, "unable to schedule the stale self-budget cleanup")
		os.Exit(1)
	}

	// The controllers start once the operator's own admission webhooks can be
	// reached (webhook_gate.go). Everything registered through ctrlMgr waits for
	// that. The collectors and the stream subscriptions write no object a webhook
	// admits, so they start with the manager.
	webhookGate := newWebhookGate(mgr.GetAPIReader(), operatorNamespace, utils.WebhookServiceName,
		ctrl.Log.WithName("webhook-gate"))
	if err := mgr.Add(webhookGate); err != nil {
		setupLog.Error(err, "unable to add the webhook gate")
		os.Exit(1)
	}
	ctrlMgr := gatedManager{Manager: mgr, gate: webhookGate.open}

	cpSubscriptions := cpinformer.NewSubscriptionManager(streamCfg, ctrl.Log.WithName("cpinformer"), cpinformer.LeaderOnly)
	deviceSubscription := subscriptions.NewDeviceSubscription()
	deviceScopes := cpSubscriptions.AddSubscription(deviceSubscription)
	// The cluster stream is the one subscription with no scope: the control
	// plane serves every cluster from one route, so a single stream covers
	// every StorageCluster in every namespace and no reconciler opens or
	// closes it. Its scope is added here, once, because there is no object
	// whose arrival would.
	clusterSubscription := subscriptions.NewClusterSubscription()
	cpSubscriptions.AddSubscription(clusterSubscription).Add(subscriptions.RootScope)
	// The task stream is scoped per cluster and carries what that cluster is
	// currently busy with, which its status publishes as a capped window.
	taskSubscription := subscriptions.NewTaskSubscription()
	taskScopes := cpSubscriptions.AddSubscription(taskSubscription)
	// One stream per cluster carries every node of it, so the cluster's own
	// controller opens the scope while the node's controller supplies the
	// backend-id-to-object mapping the events are named by.
	nodeSubscription := subscriptions.NewNodeSubscription()
	nodeScopes := cpSubscriptions.AddSubscription(nodeSubscription)
	// The data-protection band's two streams, both scoped per cluster. The
	// backup stream is the only source of a StorageBackup object, so the cluster
	// that opens it is also what registers the namespace its objects belong in.
	backupSubscription := subscriptions.NewBackupSubscription()
	backupScopes := cpSubscriptions.AddSubscription(backupSubscription)
	backupPolicySubscription := subscriptions.NewBackupPolicySubscription()
	backupPolicyScopes := cpSubscriptions.AddSubscription(backupPolicySubscription)
	// How full a node is exists only in the metrics the control plane exports,
	// so it comes from Prometheus rather than from the API or the stream. An
	// endpoint that cannot be reached leaves the capacity absent from the
	// status and everything else in it correct.
	var nodeCapacity nodecontroller.NodeCapacitySource
	if provider, err := atlasprom.New(prometheusURL); err != nil {
		setupLog.Error(err, "storage-node capacity will be absent", "prometheusURL", prometheusURL)
	} else {
		nodeCapacity = provider
	}
	// The export controller reaches its host over csi-link. It runs either
	// way: with the link off an export binds a host and then waits, visibly,
	// rather than the kind disappearing.
	nfsExportReconciler := &controller.NFSExportReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Recorder:          mgr.GetEventRecorder("nfsexport-controller"),
		OperatorNamespace: operatorNamespace,
	}
	if csiPeers != nil {
		nfsExportReconciler.Assembler = controller.NewLinkAssembler(csiPeers)
		nfsExportReconciler.Flows = csilink.NewFlows(csilink.RegistryNodePeers(csiPeers))
	}
	if err := nfsExportReconciler.SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NFSExport")
		os.Exit(1)
	}
	if err := (&nodecontroller.StorageDeviceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Devices:  deviceSubscription,
		Recorder: mgr.GetEventRecorder("storagedevice-controller"),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageDevice")
		os.Exit(1)
	}
	// What a device holds is neither desired state nor an event: it moves
	// continuously and comes from the same Prometheus the node capacity does. The
	// collector publishes it as a gauge on a timer and warns about a device over
	// its cluster's threshold, without writing any of it to an object.
	var deviceCapacity nodecontroller.DeviceCapacitySource
	if provider, err := atlasprom.New(prometheusURL); err != nil {
		setupLog.Error(err, "storage-device capacity will be absent", "prometheusURL", prometheusURL)
	} else {
		deviceCapacity = provider
	}
	if err := mgr.Add(&nodecontroller.StorageDeviceCollector{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorder("storagedevice-collector"),
		Capacity: deviceCapacity,
	}); err != nil {
		setupLog.Error(err, "unable to add the storage device collector")
		os.Exit(1)
	}
	if err := mgr.Add(cpSubscriptions); err != nil {
		setupLog.Error(err, "unable to add control-plane subscription manager")
		os.Exit(1)
	}

	// The volume stream is a second manager rather than another subscription on
	// the first, because it must run on every replica: it feeds no reconciler and
	// writes nothing, and its cache answers aggregated-API reads that arrive at
	// whichever replica the Service picked. Leader election is a property of a
	// Runnable, so the two elections need two managers.
	var volumeSubscription *subscriptions.VolumeSubscription
	var volumeScopes *cpinformer.ScopeSet
	if enableMetricsAPI {
		replicaSubscriptions := cpinformer.NewSubscriptionManager(
			streamCfg, ctrl.Log.WithName("cpinformer-replica"), cpinformer.EveryReplica)
		volumeSubscription = subscriptions.NewVolumeSubscription()
		volumeScopes = replicaSubscriptions.AddSubscription(volumeSubscription)
		if err := mgr.Add(replicaSubscriptions); err != nil {
			setupLog.Error(err, "unable to add the per-replica control-plane subscription manager")
			os.Exit(1)
		}
	}

	// A pool's occupancy is the same kind of number as a device's, read one
	// level up: it moves with every volume written to the pool, comes from the
	// same Prometheus, and is worth neither an etcd write nor a reconcile. The
	// collector also raises CapacityExhausted, which is the one pool event that
	// needs a measurement to decide.
	//
	// It is registered after the volume stream because it reads that cache for
	// the pool's logical-volume count, and the field is left unset when the
	// stream is not running: a typed nil put into the interface would be a
	// non-nil interface over a nil cache, which panics on the first pass.
	var poolCapacity pool.CapacitySource
	if provider, err := atlasprom.New(prometheusURL); err != nil {
		setupLog.Error(err, "storage-pool capacity will be absent", "prometheusURL", prometheusURL)
	} else {
		poolCapacity = provider
	}
	poolCollector := &pool.Collector{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorder("storagepool-collector"),
		Capacity: poolCapacity,
	}
	if volumeSubscription != nil {
		poolCollector.Volumes = volumeSubscription
	}
	if err := mgr.Add(poolCollector); err != nil {
		setupLog.Error(err, "unable to add the storage pool collector")
		os.Exit(1)
	}

	// Where the control plane is, read from the ControlPlane object rather than
	// from the environment (design-controlplane.md §3.3). Every control-plane
	// client below shares it, so an external control plane is reachable and a
	// change to its endpoint reaches every caller without a rollout.
	controlPlaneEndpoint := controlplanecontroller.NewEndpointResolver(
		mgr.GetClient(), operatorNamespace)

	// What a managed control plane's CreateCluster and ClusterByName calls
	// authenticate with, read from the same ControlPlane object
	// (spec.source.managed.credentialsSecretRef) for the same reason: a
	// StorageCluster CR applied against a remote control plane has no other
	// credential to create its backend identity with.
	controlPlaneCredential := controlplanecontroller.NewCredentialResolver(
		mgr.GetClient(), operatorNamespace)

	if err := (&controlplanecontroller.ControlPlaneReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("controlplane-controller"),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ControlPlane")
		os.Exit(1)
	}
	if err := (&controlplanecontroller.ControlPlaneOpsReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("controlplaneops-controller"),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ControlPlaneOps")
		os.Exit(1)
	}
	// The site link is the elected replica's, so it runs as a leader-election
	// runnable and the StorageCluster controller probes through it.
	var siteLinkProber clustercontroller.SiteLinkProber
	// peerSite reaches the other site's operator for the leader's rolling
	// restart (design-stretched-sync-replication-ramen.md §11.3); nil without a
	// site link.
	var peerSite clustercontroller.PeerSite
	if siteLinkEnabled {
		siteKube, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			setupLog.Error(err, "unable to create the site link's client")
			os.Exit(1)
		}
		siteLink := &sitelink.Link{
			Pod:         os.Getenv("POD_NAME"),
			PodUID:      os.Getenv("POD_UID"),
			Address:     siteLinkAddr,
			CANamespace: siteLinkNamespace,
			Kube:        siteKube,
			Token:       link.TokenFile(siteLinkTokenPath),
			Logger:      slog.Default().With("component", "site-link"),
			// The other site's operator asks this one to move volumes and to
			// refresh this site's storage-node pods.
			Register: func(srv grpc.ServiceRegistrar) {
				switchoverServer.Register(srv)
				(&sitesvc.Server{Client: mgr.GetClient(), Namespace: operatorNamespace}).Register(srv)
			},
		}
		if err := mgr.Add(siteLink); err != nil {
			setupLog.Error(err, "unable to add the site link")
			os.Exit(1)
		}
		siteLinkProber = siteLink
		peerSite = siteLink
	}

	if err := (&clustercontroller.StorageClusterReconciler{
		SiteLink:     siteLinkProber,
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Recorder:     mgr.GetEventRecorder("storagecluster-controller"),
		API:          clustercontroller.NewControlPlane(controlPlaneEndpoint, controlPlaneCredential),
		Namespace:    operatorNamespace,
		Clusters:     clusterSubscription,
		Tasks:        taskSubscription,
		NodeScopes:   nodeScopes,
		TaskScopes:   taskScopes,
		BackupScopes: []*cpinformer.ScopeSet{backupScopes, backupPolicyScopes},
		// Every subscription that names a StorageCluster needs the same
		// mapping, and this reconciler is the only party that knows both the
		// backend id and the object it was adopted as.
		BackupRegistrars: []clustercontroller.ClusterRegistrar{
			&clusterSubscription.ClusterRegistry,
			&taskSubscription.ClusterRegistry,
			&backupSubscription.ClusterRegistry,
			&backupPolicySubscription.ClusterRegistry,
		},
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageCluster")
		os.Exit(1)
	}
	if err := (&nodecontroller.StorageNodeWorkloadReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorder("storagenode-workload-controller"),
		Namespace: operatorNamespace,
		TLS:       controlPlaneTLS,
		Workload:  storageNodeWorkload,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageNodeWorkload")
		os.Exit(1)
	}
	if err := (&pool.StoragePoolReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Recorder:         mgr.GetEventRecorder("storagepool-controller"),
		VolumeScopes:     volumeScopes,
		EndpointResolver: controlPlaneEndpoint,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StoragePool")
		os.Exit(1)
	}
	if err := (&pool.StoragePoolOpsReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("storagepoolops-controller"),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StoragePoolOps")
		os.Exit(1)
	}
	if err := (&controller.TaskReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Task")
		os.Exit(1)
	}
	// The data-protection band. The mirror is what creates every StorageBackup
	// object, so nothing here takes a backup: a policy tells the control plane
	// to, and the operations kind reads one back into a claim.
	backupAPIConfig, err := webapi.ControlPlaneConfig()
	if err != nil {
		setupLog.Error(err, "unable to resolve the control plane the backup band writes to")
		os.Exit(1)
	}
	backupAPI, err := controlplane.New(backupAPIConfig)
	if err != nil {
		setupLog.Error(err, "unable to build the control-plane client the backup band writes through")
		os.Exit(1)
	}
	if err := (&backupcontrollers.StorageBackupReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Backups:  backupSubscription,
		Recorder: mgr.GetEventRecorder("storagebackup-controller"),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageBackup")
		os.Exit(1)
	}
	if err := (&backupcontrollers.StorageBackupRequestReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("storagebackuprequest-controller"),
		API:      backupAPI,
		Backups:  backupSubscription,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageBackupRequest")
		os.Exit(1)
	}
	if err := (&backupcontrollers.StorageGroupBackupReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("storagegroupbackup-controller"),
		API:      backupAPI,
		Backups:  backupSubscription,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageGroupBackup")
		os.Exit(1)
	}
	if err := (&backupcontrollers.StorageBackupPolicyReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("storagebackuppolicy-controller"),
		API:      backupAPI,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageBackupPolicy")
		os.Exit(1)
	}
	// An operation stored with string references cannot be decoded, and a cache
	// that cannot list it never syncs, which would exit the whole manager. The
	// controller is skipped instead, and the rest of the operator runs.
	legacyOps, err := backupcontrollers.LegacyBackupOps(context.Background(), mgr.GetAPIReader())
	if err != nil {
		setupLog.Error(err, "unable to check for StorageBackupOps in the old format; starting the controller anyway")
	}
	if len(legacyOps) > 0 {
		setupLog.Error(nil, "StorageBackupOps controller not started: these operations use the old string "+
			"spec.backupRef and spec.clusterRef. Delete them and restart the operator",
			"operations", legacyOps)
	} else if err := (&backupcontrollers.StorageBackupOpsReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorder("storagebackupops-controller"),
		API:       backupAPI,
		Placement: placementService,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageBackupOps")
		os.Exit(1)
	}
	if err := (&backupcontrollers.BackupRestoreReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("backuprestore-controller"),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BackupRestore")
		os.Exit(1)
	}
	if err := (&backupcontrollers.BackupPolicyReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("backuppolicy-controller"),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BackupPolicy")
		os.Exit(1)
	}
	if err := (&backupcontrollers.BackupImportReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("backupimport-controller"),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BackupImport")
		os.Exit(1)
	}
	// The kind a volume move is raised as, decided once and handed to all three
	// controllers that raise one. None of them has any business knowing which.
	volumeMover := volumemigration.NewMover(
		mgr.GetClient(), mgr.GetScheme(), legacyVolumeMigration)

	// The volume band. It shares the backup band's control-plane client because
	// there is one control plane and one endpoint; what differs is which of its
	// endpoints each band calls.
	if err := (&volumecontrollers.PersistentVolumeOpsReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("persistentvolumeops-controller"),
		API:      backupAPI,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PersistentVolumeOps")
		os.Exit(1)
	}
	// The registered kind's reconciler runs only where the registered kind is
	// what raises moves. Registering it regardless would cost nothing while
	// nothing creates one, and would quietly become the thing that reconciles a
	// VolumeMigration somebody applied by hand against an operator that is
	// otherwise driving the redesigned kind.
	if legacyVolumeMigration {
		if err := (&volumecontrollers.VolumeMigrationReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: mgr.GetEventRecorder("volumemigration-controller"),
		}).SetupWithManager(ctrlMgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "VolumeMigration")
			os.Exit(1)
		}
		setupLog.Info("volume moves are raised as the registered VolumeMigration kind")
	}
	if err := (&controller.PersistentVolumeClaimReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("pinnedvolume-controller"),
		Mover:    volumeMover,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PersistentVolumeClaim")
		os.Exit(1)
	}
	storageNodeReconciler := &nodecontroller.StorageNodeReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("storagenode-controller"),
		API:      nodecontroller.NewControlPlane(controlPlaneEndpoint),
		Nodes:    nodeSubscription,
		Registries: []nodecontroller.NodeObjectRegistry{
			deviceSubscription, nodeSubscription,
		},
		DeviceScopes: deviceScopes,
		Capacity:     nodeCapacity,
		Workload:     storageNodeWorkload,
	}
	// The host checks go to the CSI node plugins over the link. Without it a
	// node on a network waits for them.
	if csiPeers != nil {
		storageNodeReconciler.HostCheck = csilink.NewHostCheck(csilink.RegistryNodePeers(csiPeers))
	}
	if err := storageNodeReconciler.SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageNode")
		os.Exit(1)
	}
	if err := (&nodecontroller.RemoteNodeReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("storagenode-remote-controller"),
		API:      nodecontroller.NewControlPlane(controlPlaneEndpoint),
		Nodes:    nodeSubscription,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageNodeRemote")
		os.Exit(1)
	}
	if err := (&nodecontroller.StorageNodeOpsReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("storagenodeops-controller"),
		API:      nodecontroller.NewControlPlane(controlPlaneEndpoint),
		Nodes:    nodeSubscription,
		Clusters: clusterSubscription,
		Workload: storageNodeWorkload,
		Mover:    volumeMover,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageNodeOps")
		os.Exit(1)
	}
	// The distribution a cluster runs is concluded partly from the API groups it
	// registers, and a run without this client concludes it from the nodes
	// alone rather than failing.
	operatorOpsDiscovery, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to build a discovery client; "+
			"a discovery run will read the distribution from the nodes alone")
	}

	// The discovery run's probe Jobs run the operator's own image, so that a Job
	// cannot be a version out of step with the operator that created it. The
	// image is read from the environment rather than from the running pod,
	// because a pod may name its image by a tag the registry has since moved and
	// what a Job needs is the reference the operator was deployed with.
	if err := (&deployment.ClusterDeploymentConfigReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorder("clusterdeploymentconfig-controller"),
		Namespace: operatorNamespace,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterDeploymentConfig")
		os.Exit(1)
	}
	if err := (&deployment.OperatorOpsReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Recorder:   mgr.GetEventRecorder("operatorops-controller"),
		Discovery:  operatorOpsDiscovery,
		ProbeImage: os.Getenv(deployment.NodeProbeImageEnv),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "OperatorOps")
		os.Exit(1)
	}
	// Whether the cluster serves the snapshot API decides both whether a
	// VolumeSnapshotClass is applied and what status.snapshotSupport records
	// (design-simplyblockdriver.md §4.1). The discovery client is the same one
	// the discovery run uses; a driver reconcile without it refuses rather than
	// guessing, because guessing either way breaks a cluster in one direction.
	// One device is the narrowest thing that can be recycled, and one device is
	// the narrowest thing that can be taken out of the data path, which is the
	// whole of why the kind exists (design-storagedevice.md §6). Its three other
	// actions wait on control-plane verbs the v2 API does not offer, and the
	// TODO beside their constants in storagedeviceops_types.go is the list.
	// Device health: each device measured against the others of its cluster
	// and its health log read on a schedule, both published on the device and
	// reported to the control plane, whose alerts endpoint raises them. The
	// CheckHealth operation reads a log through the same monitor, so a read on
	// demand is judged and reported exactly as a scheduled one.
	var deviceHealth *nodecontroller.StorageDeviceHealthMonitor
	if enableDeviceHealth {
		deviceHealth = &nodecontroller.StorageDeviceHealthMonitor{
			Client:        mgr.GetClient(),
			Recorder:      mgr.GetEventRecorder("storagedevice-health"),
			API:           backupAPI,
			Interval:      deviceHealthInterval,
			Window:        deviceHealthWindow,
			SMARTInterval: deviceSMARTInterval,
		}
		if provider, err := atlasprom.New(prometheusURL); err != nil {
			setupLog.Error(err, "storage-device performance will not be assessed", "prometheusURL", prometheusURL)
		} else {
			deviceHealth.IO = provider
		}
		if err := mgr.Add(deviceHealth); err != nil {
			setupLog.Error(err, "unable to add the storage device health monitor")
			os.Exit(1)
		}
	}
	deviceOps := &nodecontroller.StorageDeviceOpsReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("storagedeviceops-controller"),
		API:      backupAPI,
	}
	if deviceHealth != nil {
		deviceOps.Health = deviceHealth
	}
	if err := deviceOps.SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageDeviceOps")
		os.Exit(1)
	}
	// The CSI controller joins the operator's site link, so the other site's
	// operator can reach it too.
	var driverSiteLink *driver.SiteLinkConfig
	if siteLinkEnabled {
		driverSiteLink = &driver.SiteLinkConfig{Address: siteLinkAddr, AgentNamespace: siteLinkNamespace}
	}
	if err := (&driver.SimplyblockDriverReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorder("simplyblockdriver-controller"),
		Snapshots: &driver.DiscoveredSnapshotAPI{Discovery: operatorOpsDiscovery},
		SiteLink:  driverSiteLink,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SimplyblockDriver")
		os.Exit(1)
	}
	if err := (&clustercontroller.StorageClusterOpsReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("storageclusterops-controller"),
		API:      clustercontroller.NewControlPlane(controlPlaneEndpoint, controlPlaneCredential),
		Clusters: clusterSubscription,
		Nodes:    nodeSubscription,
		Tasks:    taskSubscription,
		Peer:     peerSite,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageClusterOps")
		os.Exit(1)
	}
	// A member's capacity exists only in the metrics the control plane exports
	// (design-superclusters.md §16 Q13). An endpoint that cannot be reached
	// leaves the supercluster's capacity unreported and the rest of its status
	// correct.
	var memberCapacity clustercontroller.MemberCapacitySource
	if provider, err := atlasprom.New(prometheusURL); err != nil {
		setupLog.Error(err, "supercluster capacity will be absent", "prometheusURL", prometheusURL)
	} else {
		memberCapacity = provider
		placementService.Capacity = provider
	}
	placementLookupConfig, err := webapi.ControlPlaneConfig()
	if err != nil {
		setupLog.Error(err, "unable to resolve the control plane placement asks")
		os.Exit(1)
	}
	placementService.Lookup = placement.ControlPlaneLookup{Reader: mgr.GetClient(), Config: placementLookupConfig}
	if err := (&clustercontroller.SuperclusterReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("supercluster-controller"),
		Capacity: memberCapacity,
		Readings: placementService,
		TLS:      controlPlaneTLS,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Supercluster")
		os.Exit(1)
	}
	if err := (&controller.VolumeRebalancerReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Recorder:          mgr.GetEventRecorder("volumerebalancer-controller"),
		LatencyPercentile: latencyPercentile,
		Mover:             volumeMover,
	}).SetupWithManager(ctrlMgr); err != nil {
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
		// Resolves each storage node's data-network IP (the NICs endpoint) for the fio baseline;
		// independent of the provisioner, so it is set for both prod and test paths.
		APIClient: webapi.NewClient(),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageNodeLatency")
		os.Exit(1)
	}
	if err := (&controller.ReplicationPolicyReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		EndpointResolver: controlPlaneEndpoint,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ReplicationPolicy")
		os.Exit(1)
	}
	if err := (&controller.PVCAnnotationWatcher{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PVCAnnotationWatcher")
		os.Exit(1)
	}
	if err := (&controller.ReplicationPairReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		EndpointResolver: controlPlaneEndpoint,
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ReplicationPair")
		os.Exit(1)
	}
	// Two-node arbitration (sbcli docs/design/two-node-arbitration.md §9): follows the
	// arbiter's record and taints the Kubernetes node of a fenced storage node.
	if err := (&twonode.ArbitrationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		API:    &twonodecp.Adapter{Reader: mgr.GetClient(), EndpointResolver: controlPlaneEndpoint},
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TwoNodeArbitration")
		os.Exit(1)
	}
	// NodeHealthCheck's external remediation for a storage-fenced node (sbcli
	// docs/design/two-node-kubernetes-fencing.md §1): the out-of-service taint.
	if err := (&twonode.RemediationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		API:    &twonodecp.Adapter{Reader: mgr.GetClient(), EndpointResolver: controlPlaneEndpoint},
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StorageFenceRemediation")
		os.Exit(1)
	}
	if err := (&controller.ReplicationSlotReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("replicationslot-controller"),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ReplicationSlot")
		os.Exit(1)
	}
	if err := (&controller.ReplicationOpsReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("replicationops-controller"),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ReplicationOps")
		os.Exit(1)
	}
	if err := (&consistencygroupcontrollers.VolumeGroupSnapshotOpsReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("volumegroupsnapshotops-controller"),
	}).SetupWithManager(ctrlMgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VolumeGroupSnapshotOps")
		os.Exit(1)
	}
	// The TestFailover controller watches OCM ManifestWork, which only the hub
	// serves; registering it where the work API is absent leaves a watch whose
	// cache never syncs and the manager exits at startup, taking every other
	// controller with it. It is a hub-only drill, so skip it off the hub.
	workDiscovery, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to build a discovery client to check for the OCM work API")
		os.Exit(1)
	}
	hasManifestWork, err := serverHasResource(workDiscovery, ocmWorkGroupVersion, ocmManifestWorkResource)
	if err != nil {
		setupLog.Error(err, "unable to determine whether the OCM ManifestWork resource is served")
		os.Exit(1)
	}
	registerOCMControllers := func() error {
		if err := (&controller.TestFailoverReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: mgr.GetEventRecorder("testfailover-controller"),
		}).SetupWithManager(ctrlMgr); err != nil {
			return fmt.Errorf("controller TestFailover: %w", err)
		}
		// A managed site's storage deployment is requested from the hub through
		// the same work API, so the controller is hub-only too.
		if err := (&controller.StorageSiteDeploymentReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: mgr.GetEventRecorder("storagesitedeployment-controller"),
		}).SetupWithManager(ctrlMgr); err != nil {
			return fmt.Errorf("controller StorageSiteDeployment: %w", err)
		}
		// A stretched cluster is two site deployments joined on the hub, which
		// also runs the control plane the follower's credentials are read from.
		if err := (&controller.StretchedClusterReconciler{
			Client:       mgr.GetClient(),
			Scheme:       mgr.GetScheme(),
			Recorder:     mgr.GetEventRecorder("stretchedcluster-controller"),
			ControlPlane: webapi.NewClient(),
		}).SetupWithManager(ctrlMgr); err != nil {
			return fmt.Errorf("controller StretchedCluster: %w", err)
		}
		return nil
	}
	if hasManifestWork {
		if err := registerOCMControllers(); err != nil {
			setupLog.Error(err, "unable to create controller")
			os.Exit(1)
		}
	} else {
		// OCM may be installed after the operator (the DR stack brings it): keep
		// looking, and start the controllers once ManifestWork is served.
		setupLog.Info("OCM ManifestWork resource not served yet; the TestFailover, "+
			"StorageSiteDeployment, and StretchedCluster controllers start once it is (hub-only)",
			"groupVersion", ocmWorkGroupVersion, "resource", ocmManifestWorkResource)
		if err := mgr.Add(&ocmLateStart{log: setupLog, disc: workDiscovery, register: registerOCMControllers}); err != nil {
			setupLog.Error(err, "unable to add the OCM ManifestWork watcher")
			os.Exit(1)
		}
	}
	// The VRG follower moves a stretched cluster's volumes to the site Ramen makes
	// their VolumeReplicationGroup primary on (design-stretched-sync-replication-
	// ramen.md §9). It watches Ramen's kind, so it starts once Ramen is installed.
	registerVRGFollower := func() error {
		if err := (&vrgfollow.Reconciler{
			Client:    mgr.GetClient(),
			Recorder:  mgr.GetEventRecorder("vrgfollow"),
			Mover:     switchoverServer,
			Namespace: operatorNamespace,
		}).SetupWithManager(ctrlMgr); err != nil {
			return fmt.Errorf("controller VRGFollower: %w", err)
		}
		return nil
	}
	hasVRG, err := serverHasResource(workDiscovery, ramenGroupVersion, ramenVRGResource)
	if err != nil {
		setupLog.Error(err, "unable to determine whether Ramen's VolumeReplicationGroup is served")
		os.Exit(1)
	}
	if hasVRG {
		if err := registerVRGFollower(); err != nil {
			setupLog.Error(err, "unable to create controller")
			os.Exit(1)
		}
	} else if err := mgr.Add(&ramenLateStart{
		log: setupLog, disc: workDiscovery, register: registerVRGFollower,
	}); err != nil {
		setupLog.Error(err, "unable to add the Ramen VolumeReplicationGroup watcher")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	// Provision the admission webhooks' serving certificate at runtime (self-signed
	// via cert-controller, or from cert-manager when SB_TLS_PROVIDER=cert-manager).
	// Conversion is not served here: it runs as its own Deployment
	// (design-api-upgrade.md §6.1, cmd/conversion-webhook).
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

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-storagenode",
			&webhook.Admission{Handler: &internalwebhook.StorageNodeValidator{
				Client:            mgr.GetClient(),
				OperatorNamespace: operatorNamespace,
			}})
		setupLog.Info("registered storagenode validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha1-replicationops",
			&webhook.Admission{Handler: &internalwebhook.ReplicationOpsValidator{Client: mgr.GetClient()}})
		setupLog.Info("registered replicationops validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-simplyblockdriver",
			&webhook.Admission{Handler: &internalwebhook.SimplyblockDriverValidator{Client: mgr.GetClient()}})
		setupLog.Info("registered simplyblockdriver validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-storagedevice",
			&webhook.Admission{Handler: &internalwebhook.StorageDeviceValidator{
				Client:            mgr.GetClient(),
				OperatorNamespace: operatorNamespace,
			}})
		setupLog.Info("registered storagedevice validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-storagecluster",
			&webhook.Admission{Handler: &internalwebhook.StorageClusterValidator{
				Client:  mgr.GetClient(),
				Decoder: admission.NewDecoder(mgr.GetScheme()),
			}})
		setupLog.Info("registered storagecluster validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-storagepool",
			&webhook.Admission{Handler: &internalwebhook.StoragePoolValidator{
				Client:  mgr.GetClient(),
				Decoder: admission.NewDecoder(mgr.GetScheme()),
			}})
		setupLog.Info("registered storagepool validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-clusterdeploymentconfig",
			&webhook.Admission{Handler: &internalwebhook.ClusterDeploymentConfigValidator{
				Client:  mgr.GetClient(),
				Decoder: admission.NewDecoder(mgr.GetScheme()),
			}})
		setupLog.Info("registered clusterdeploymentconfig validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-storagebackup",
			&webhook.Admission{Handler: &internalwebhook.StorageBackupValidator{
				Client:            mgr.GetClient(),
				OperatorNamespace: operatorNamespace,
			}})
		setupLog.Info("registered storagebackup validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-storagebackuppolicy",
			&webhook.Admission{Handler: &internalwebhook.StorageBackupPolicyValidator{Client: mgr.GetClient()}})
		setupLog.Info("registered storagebackuppolicy validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-storagebackupops",
			&webhook.Admission{Handler: &internalwebhook.StorageBackupOpsValidator{Client: mgr.GetClient()}})
		setupLog.Info("registered storagebackupops validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-controlplaneops",
			&webhook.Admission{Handler: &internalwebhook.ControlPlaneOpsValidator{Client: mgr.GetClient()}})
		setupLog.Info("registered controlplaneops validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-storageclusterops",
			&webhook.Admission{Handler: &internalwebhook.StorageClusterOpsValidator{Client: mgr.GetClient()}})
		setupLog.Info("registered storageclusterops validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-operatorops",
			&webhook.Admission{Handler: &internalwebhook.OperatorOpsValidator{}})
		setupLog.Info("registered operatorops validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-storagenodeops",
			&webhook.Admission{Handler: &internalwebhook.StorageNodeOpsValidator{Client: mgr.GetClient()}})
		setupLog.Info("registered storagenodeops validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-stretchedcluster",
			// The role-swap guard reads the control plane the hub runs, which is
			// the current record of a lost site and its fence.
			&webhook.Admission{Handler: &internalwebhook.StretchedClusterValidator{
				Backend: clustercontroller.NewControlPlane(controlPlaneEndpoint, controlPlaneCredential),
			}})
		setupLog.Info("registered stretchedcluster validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-storagedeviceops",
			&webhook.Admission{Handler: &internalwebhook.StorageDeviceOpsValidator{Client: mgr.GetClient()}})
		setupLog.Info("registered storagedeviceops validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-persistentvolumeops",
			&webhook.Admission{Handler: &internalwebhook.PersistentVolumeOpsValidator{
				Client: mgr.GetClient(),
			}})
		setupLog.Info("registered persistentvolumeops validating webhook")

		mgr.GetWebhookServer().Register("/validate-v1-pvc-pinned-volume",
			&webhook.Admission{Handler: &internalwebhook.PersistentVolumeClaimValidator{
				Client:    mgr.GetClient(),
				APIClient: webapi.NewClient(),
			}})
		setupLog.Info("registered pinned-volume validating webhook")

		mgr.GetWebhookServer().Register("/validate-v1-pvc-vdo-size",
			&webhook.Admission{Handler: &internalwebhook.VDOSizeFloorValidator{Client: mgr.GetClient()}})
		setupLog.Info("registered vdo-size-floor validating webhook")

		mgr.GetWebhookServer().Register("/mutate-v1-pvc-simplyblock-placement",
			&webhook.Admission{Handler: &internalwebhook.SimplyblockVolumePlacementInjector{
				Client:       mgr.GetClient(),
				APIClient:    webapi.NewClient(),
				NodeSelector: autoplacement.NewStorageNodeSelector(mgr.GetClient()),
			}})
		setupLog.Info("registered simplyblock-volume-placement mutating webhook")

		mgr.GetWebhookServer().Register("/validate-groupsnapshot-storage-k8s-io-v1beta1-volumegroupsnapshot",
			&webhook.Admission{Handler: &internalwebhook.VolumeGroupSnapshotValidator{
				Client:    mgr.GetClient(),
				APIClient: webapi.NewClient(),
			}})
		setupLog.Info("registered volumegroupsnapshot validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha2-volumegroupsnapshotops",
			&webhook.Admission{Handler: &internalwebhook.VolumeGroupSnapshotOpsValidator{Client: mgr.GetClient()}})
		setupLog.Info("registered volumegroupsnapshotops validating webhook")

		mgr.GetWebhookServer().Register("/validate-storage-simplyblock-io-v1alpha1-volumemigration",
			&webhook.Admission{Handler: &internalwebhook.VolumeMigrationValidator{
				Client:    mgr.GetClient(),
				APIClient: webapi.NewClient(),
			}})
		setupLog.Info("registered volumemigration validating webhook")
	}()

	// The aggregated metrics API: LogicalVolumeMetrics served from the volume
	// cache above joined to the claims that name them, and StorageDeviceMetrics
	// from the device objects joined to what Prometheus last measured of them.
	if enableMetricsAPI {
		err := metricsapi.Install(
			mgr, operatorNamespace, metricsAPIPort, volumeSubscription, prometheusURL,
		)
		if err != nil {
			setupLog.Error(err, "unable to install the aggregated metrics API")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// Ready only while the admission webhooks serve: readiness is what puts the
	// pod into the webhook Service's endpoints, and the controllers wait on those.
	webhookServing := webhookReadyCheck(webhookReady, func() healthz.Checker {
		return mgr.GetWebhookServer().StartedChecker()
	})
	if err := mgr.AddReadyzCheck("readyz", webhookServing); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// leaderOnly wraps a one-shot task the manager runs after it wins the leader
// election, and not before.
//
// The distinction matters for anything that cleans up after a previous
// instance: a replica that ran it on the way up would be undoing what the
// current leader is in the middle of.
type leaderOnly func(context.Context) error

func (f leaderOnly) Start(ctx context.Context) error { return f(ctx) }

func (leaderOnly) NeedLeaderElection() bool { return true }

// newSwitchoverServer builds the operator's SwitchoverService. A move that had
// to be forced, because the site the volume was served from is not online, is
// announced on this namespace's StorageCluster as SyncPeerSiteOffline.
func newSwitchoverServer(mgr ctrl.Manager, namespace string) *atlasswitchover.Server {
	backend := &switchover.Backend{
		Endpoint:   controlplanecontroller.NewEndpointResolver(mgr.GetClient(), namespace),
		Credential: controlplanecontroller.NewCredentialResolver(mgr.GetClient(), namespace),
	}
	if cfg, err := webapi.ControlPlaneConfig(); err == nil {
		backend.Transport = cfg.Transport
	}
	server := atlasswitchover.New(backend, slog.Default().With("component", "switchover"))
	recorder := mgr.GetEventRecorder("switchover")
	server.OnForced = func(volume, from, to string) {
		var clusters simplyblockv1alpha2.StorageClusterList
		if err := mgr.GetClient().List(context.Background(), &clusters, client.InNamespace(namespace)); err != nil {
			return
		}
		for i := range clusters.Items {
			recorder.Eventf(&clusters.Items[i], nil, corev1.EventTypeWarning, "SyncPeerSiteOffline", "SyncPeerSiteOffline",
				"volume %s was promoted on site %s by force: site %s, which served it, is not online", volume, to, from)
		}
	}
	return server
}
