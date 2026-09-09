// Tests for the connection Helm is given.
//
// The failure these exist for is silent and only happens on someone else's
// machine: a Helm configuration built the usual way re-resolves KUBECONFIG and
// the current context, so a run that inspected the cluster named by --context
// would release to whichever cluster the environment happened to point at. It
// works whenever the two agree, which is most of the time.

package helm

import (
	"os"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func TestGetter_AnswersWithTheConnectionItWasGiven(t *testing.T) {
	config := &rest.Config{Host: "https://the-cluster-we-were-told-about:6443"}

	got, err := (&getter{config: config, namespace: "simplyblock"}).ToRESTConfig()
	if err != nil {
		t.Fatalf("ToRESTConfig: %v", err)
	}
	if got != config {
		t.Fatalf("host = %q, want the connection the run already holds", got.Host)
	}
}

func TestGetter_IgnoresTheEnvironment(t *testing.T) {
	// The whole point. A getter that fell back to the ambient kubeconfig would
	// work on every machine where the flags happened to match it, and release
	// to the wrong cluster on the rest.
	t.Setenv("KUBECONFIG", "/nonexistent/kubeconfig-that-would-fail-if-read")

	config := &rest.Config{Host: "https://the-cluster-we-were-told-about:6443"}
	g := &getter{config: config, namespace: "simplyblock"}

	got, err := g.ToRESTConfig()
	if err != nil {
		t.Fatalf("ToRESTConfig: %v", err)
	}
	if got.Host != config.Host {
		t.Fatalf("host = %q with KUBECONFIG set, want the connection we were given", got.Host)
	}

	// The namespace loader is the other way in, and it is built over an empty
	// configuration held in memory rather than over loading rules that can
	// reach a file.
	namespace, _, err := g.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		t.Fatalf("Namespace: %v", err)
	}
	if namespace != "simplyblock" {
		t.Fatalf("namespace = %q, want the one this client was built for", namespace)
	}
}

func TestGetter_ResolvesKindsThroughTheSameConnection(t *testing.T) {
	// The mapper and the discovery client are built from the connection rather
	// than from a config file, so a kind resolved during an upgrade is
	// resolved against the cluster being upgraded.
	g := &getter{config: &rest.Config{Host: "https://example:6443"}, namespace: "simplyblock"}

	if _, err := g.ToDiscoveryClient(); err != nil {
		t.Fatalf("ToDiscoveryClient: %v", err)
	}
	if _, err := g.ToRESTMapper(); err != nil {
		t.Fatalf("ToRESTMapper: %v", err)
	}
}

func TestNew_RefusesWithoutAConnection(t *testing.T) {
	// Rather than letting Helm build its own, which is the failure this
	// package exists to prevent.
	if _, err := New(nil, "simplyblock", nil); err == nil {
		t.Fatal("a Helm client was built with no connection, and Helm would have found one")
	}
}

func TestNew_BuildsOverTheGivenConnection(t *testing.T) {
	client, err := New(&rest.Config{Host: "https://example:6443"}, "simplyblock", func(string, ...any) {})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.Namespace() != "simplyblock" {
		t.Fatalf("namespace = %q", client.Namespace())
	}
}

func TestLoadChart_ReadsTheRepositoryChart(t *testing.T) {
	// The chart this tool would upgrade to, loaded the way the upgrade step
	// will load it. It is a real chart rather than a fixture, so a template
	// that stops parsing is caught here.
	const path = "../../../../helm-charts/charts/simplyblock-operator"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("the chart is not beside this checkout: %v", err)
	}

	loaded, err := LoadChart(path)
	if err != nil {
		t.Fatalf("LoadChart: %v", err)
	}
	if loaded.Name() != "simplyblock-operator" {
		t.Errorf("chart name = %q", loaded.Name())
	}
	if len(loaded.Templates) == 0 {
		t.Error("the chart loaded with no templates")
	}
}

func TestLoadChart_SaysWhichPathItCouldNotRead(t *testing.T) {
	_, err := LoadChart("/nonexistent/chart")
	if err == nil {
		t.Fatal("loading a chart that is not there succeeded")
	}
	if !strings.Contains(err.Error(), "/nonexistent/chart") {
		t.Errorf("error = %q, want it to name the path", err)
	}
}
