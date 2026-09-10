// Building a Helm action configuration over a connection somebody else made.
//
// The type that matters is genericclioptions.RESTClientGetter, which is how
// Helm asks for a cluster. Its usual implementation reads KUBECONFIG and the
// current context, which is exactly the resolution the tool's own --kubeconfig
// and --context flags exist to override, so this one answers with the
// connection it was given and reads nothing.

package helm

import (
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Client is Helm, pointed at one cluster and one namespace.
type Client struct {
	config    *action.Configuration
	namespace string
}

// New builds a Helm client over an existing connection.
//
// The namespace is where the release lives, which for this tool is the
// operator's own (§17): the Helm release, the conversion webhook, and the
// migration record are the operator's furniture, and the custom resources it
// reconciles are elsewhere.
//
// log receives Helm's own narration, which is verbose and belongs in the
// diagnostic log rather than in the report.
func New(config *rest.Config, namespace string, log func(string, ...any)) (*Client, error) {
	if config == nil {
		return nil, fmt.Errorf("no connection to build a Helm client over")
	}

	var cfg action.Configuration
	if err := cfg.Init(&getter{config: config, namespace: namespace}, namespace, "", log); err != nil {
		return nil, fmt.Errorf("building the Helm configuration: %w", err)
	}
	return &Client{config: &cfg, namespace: namespace}, nil
}

// Namespace is where this client's releases live.
func (c *Client) Namespace() string {
	return c.namespace
}

// getter answers Helm's questions about which cluster to talk to, with the
// connection it was handed.
//
// Every method here is deliberately incapable of consulting the environment. A
// getter that fell back to the ambient kubeconfig would work on every machine
// where the flags happened to match it and release to the wrong cluster on the
// rest.
type getter struct {
	config    *rest.Config
	namespace string
}

// ToRESTConfig returns the connection this was built over.
func (g *getter) ToRESTConfig() (*rest.Config, error) {
	return g.config, nil
}

// ToDiscoveryClient returns a discovery client that caches in memory. Helm asks
// what the API server serves once per action and again per object, and an
// uncached client turns that into a request each time.
func (g *getter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	client, err := discovery.NewDiscoveryClientForConfig(g.config)
	if err != nil {
		return nil, fmt.Errorf("building a discovery client: %w", err)
	}
	return memory.NewMemCacheClient(client), nil
}

// ToRESTMapper resolves a kind to a resource, deferring the discovery it needs
// until something asks.
func (g *getter) ToRESTMapper() (meta.RESTMapper, error) {
	discoveryClient, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient), nil
}

// ToRawKubeConfigLoader answers the namespace question, from a configuration
// held in memory rather than one read from a file.
//
// Helm uses this for the namespace and for nothing else that matters here. It
// is built empty on purpose: a loader with real loading rules in it is one that
// can reach a kubeconfig, and reaching one is what this getter exists to avoid.
func (g *getter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	overrides := &clientcmd.ConfigOverrides{Context: clientcmdapi.Context{Namespace: g.namespace}}
	return clientcmd.NewDefaultClientConfig(*clientcmdapi.NewConfig(), overrides)
}

// The getter is what Helm asks for, so the compiler is asked to agree.
var _ genericclioptions.RESTClientGetter = (*getter)(nil)
