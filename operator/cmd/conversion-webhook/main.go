// The conversion webhook, as its own process.
//
// It runs independently of the operator on purpose (design-api-upgrade.md §6.1).
// A CRD whose conversion strategy is Webhook cannot be read at all while the
// webhook is unreachable, and the objects an administrator reads in order to
// diagnose a failed operator are simplyblock custom resources. Serving
// conversion from inside the operator would make those unreadable exactly when
// they are most needed, and would put the operator's own cache sync behind a
// webhook that the same process has not started yet.
//
// This process therefore runs no controllers, holds no leader-election lease,
// and reads no simplyblock custom resource. It provisions its own serving
// certificate, injects the CA into the converting CRDs, and answers /convert.
//
// It ships in the operator image under a second entry point rather than an image
// of its own, so that the conversion code and the API types it converts between
// are versioned together with the operator that reads them (§29.3).

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/tlsutil"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	internalwebhook "github.com/simplyblock/simplyblock-operator/internal/webhook"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("conversion-webhook")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	// The CA bundle is injected into the converting kinds' CRDs, so this process
	// has to know that kind.
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))

	// Both versions, because converting between them is the whole job.
	utilruntime.Must(simplyblockv1alpha1.AddToScheme(scheme))
	utilruntime.Must(simplyblockv1alpha2.AddToScheme(scheme))
}

func main() {
	var probeAddr string
	var webhookPort int
	var enableHTTP2 bool
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.IntVar(&webhookPort, "webhook-bind-port", 9443,
		"The port the conversion webhook serves /convert on.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the webhook server")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	namespace, err := tlsutil.DetectOperatorNamespace()
	if err != nil {
		setupLog.Error(err, "unable to determine the namespace this webhook runs in")
		os.Exit(1)
	}
	tlsProvider := os.Getenv("SB_TLS_PROVIDER")

	cfg := ctrl.GetConfigOrDie()

	// Everything the API server needs in order to call this webhook is in place
	// before the manager starts. The ordering matters even here, where no cache
	// syncs: the webhook server's certwatcher fails outright on a missing
	// certificate file, so the material has to be on disk first.
	if err := internalwebhook.BootstrapConversionTrust(
		context.Background(), cfg, scheme, namespace, tlsProvider,
	); err != nil {
		setupLog.Error(err, "unable to bootstrap the conversion webhook's trust")
		os.Exit(1)
	}
	setupLog.Info("bootstrapped serving certificate and CA bundle",
		"namespace", namespace, "service", utils.ConversionWebhookServiceName)

	// Disabling HTTP/2 avoids the Stream Cancellation and Rapid Reset CVEs, and
	// this server has no use for it.
	var tlsOpts []func(*tls.Config)
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			c.NextProtos = []string{"http/1.1"}
		})
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		// No controllers run here, so no cache is needed and no metrics are
		// served. A cache would be the thing §6.1 exists to avoid: it would read
		// the very kinds this process converts.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         false,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: utils.ConversionWebhookCertDir,
			TLSOpts: tlsOpts,
		}),
	})
	if err != nil {
		setupLog.Error(err, "unable to start the conversion webhook manager")
		os.Exit(1)
	}

	if err := internalwebhook.SetupConversionWebhooks(mgr); err != nil {
		setupLog.Error(err, "unable to register the conversion webhook")
		os.Exit(1)
	}
	setupLog.Info("registered /convert", "kinds", internalwebhook.ConvertedKindCRDNames())

	// The service reference the shipped CRDs carry names the default install's
	// namespace, and this process is the only party that knows where it actually
	// runs. It also has to be re-applied, because re-applying the CRDs during an
	// upgrade puts the shipped value back.
	if err := internalwebhook.SetupConversionServiceReference(mgr, namespace); err != nil {
		setupLog.Error(err, "unable to keep the conversion service reference correct")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up the health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up the readiness check")
		os.Exit(1)
	}

	setupLog.Info("starting the conversion webhook", "port", webhookPort)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "conversion webhook exited")
		os.Exit(1)
	}
}
