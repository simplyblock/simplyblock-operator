package e2e

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	ginkgo "github.com/onsi/ginkgo/v2"
	"k8s.io/klog"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/config"
)

func init() {
	klog.SetOutput(ginkgo.GinkgoWriter)

	if os.Getenv("KUBECONFIG") == "" {
		kubeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		_ = os.Setenv("KUBECONFIG", kubeConfigPath)
	}

	config.CopyFlags(config.Flags, flag.CommandLine)
	framework.RegisterCommonFlags(flag.CommandLine)
	framework.RegisterClusterFlags(flag.CommandLine)
	// framework.RegisterCommonFlags already registers the logging flags (incl. "-v")
	// on flag.CommandLine, so initialize klog on a throwaway set to avoid a
	// "flag redefined: v" panic during init.
	klog.InitFlags(flag.NewFlagSet("klog", flag.ContinueOnError))

	testing.Init()
	flag.Parse()
	framework.AfterReadingAllFlags(&framework.TestContext)
}

var _ = ginkgo.SynchronizedBeforeSuite(func() []byte {
	return []byte{}
}, func(_ []byte) {})

var _ = ginkgo.SynchronizedAfterSuite(func() {
}, func() {})
